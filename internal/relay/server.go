package relay

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/trade"
)

const maxRequestBody = 64 << 10

//go:embed web
var embeddedWeb embed.FS

type Config struct {
	Network   string
	PublicURL string
	Version   string
	Logger    *slog.Logger
	Now       func() time.Time
}

type Server struct {
	store     *Store
	network   string
	publicURL string
	version   string
	logger    *slog.Logger
	now       func() time.Time
	static    http.Handler
	index     []byte
}

func NewServer(store *Store, config Config) (*Server, error) {
	if store == nil {
		return nil, errors.New("relay store is required")
	}
	switch config.Network {
	case "mainnet", "testnet", "regtest":
	default:
		return nil, errors.New("relay network must be mainnet, testnet or regtest")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	root, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		store: store, network: config.Network, publicURL: strings.TrimRight(config.PublicURL, "/"),
		version: config.Version, logger: config.Logger, now: config.Now,
		static: http.FileServer(http.FS(root)), index: index,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/orders", s.handleOrders)
	mux.HandleFunc("POST /api/v1/orders", s.handlePublish)
	mux.HandleFunc("GET /api/v1/orders/{id}", s.handleOrder)
	mux.HandleFunc("POST /api/v1/orders/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /api/v1/orders/{id}/accept", s.handleAccept)
	mux.HandleFunc("POST /api/v1/orders/{id}/match", s.handleMatch)
	mux.HandleFunc("POST /api/v1/messages", s.handleMessage)
	mux.HandleFunc("POST /api/v1/mailbox/poll", s.handleMailboxPoll)
	mux.HandleFunc("/api/", func(response http.ResponseWriter, request *http.Request) {
		writeError(response, http.StatusNotFound, "API endpoint not found")
	})
	mux.HandleFunc("/", s.handleWeb)
	return s.securityHeaders(s.logRequests(mux))
}

func (s *Server) handleStatus(response http.ResponseWriter, _ *http.Request) {
	now := s.now().UTC()
	stats, err := s.store.Stats(now)
	if err != nil {
		s.internalError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, struct {
		Service    string `json:"service"`
		Version    string `json:"version"`
		Network    string `json:"network"`
		PublicURL  string `json:"publicURL"`
		ServerTime string `json:"serverTime"`
		Stats      Stats  `json:"stats"`
	}{"QDAY DEX", s.version, s.network, s.publicURL, now.Format(time.RFC3339), stats})
}

func (s *Server) handleOrders(response http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	page, err := positiveQueryInteger(query.Get("page"), 1, 1_000_000)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid page")
		return
	}
	limit, err := positiveQueryInteger(query.Get("limit"), 20, 100)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid limit")
		return
	}
	status := Status(query.Get("status"))
	if status == "" {
		status = StatusOpen
	}
	if status != StatusOpen && status != StatusCancelled && status != StatusExpired && status != StatusMatched && status != "all" {
		writeError(response, http.StatusBadRequest, "invalid status")
		return
	}
	market := query.Get("market")
	if market != "" && market != order.MarketQDAYBTC {
		writeError(response, http.StatusBadRequest, "unsupported market")
		return
	}
	giveAsset := query.Get("giveAsset")
	if giveAsset != "" && giveAsset != "QDAY" && giveAsset != "BTC" {
		writeError(response, http.StatusBadRequest, "giveAsset must be QDAY or BTC")
		return
	}
	result, err := s.store.List(Query{Status: status, Market: market, GiveAsset: giveAsset, Page: page, Limit: limit}, s.now())
	if err != nil {
		s.internalError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) handleAccept(response http.ResponseWriter, request *http.Request) {
	var acceptance trade.SignedAcceptance
	if err := decodeJSON(response, request, &acceptance); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if acceptance.Acceptance.OrderID != request.PathValue("id") {
		writeError(response, http.StatusBadRequest, "path order ID does not match acceptance")
		return
	}
	if acceptance.Acceptance.Network != s.network {
		writeError(response, http.StatusBadRequest, fmt.Sprintf("relay accepts only %s trades", s.network))
		return
	}
	record, created, err := s.store.SubmitAcceptance(request.PathValue("id"), acceptance, s.now())
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(response, http.StatusNotFound, ErrNotFound.Error())
	case errors.Is(err, ErrNotOpen), errors.Is(err, ErrAlreadyAccepted):
		writeError(response, http.StatusConflict, err.Error())
	case errors.Is(err, ErrAcceptanceLimit):
		writeError(response, http.StatusTooManyRequests, err.Error())
	case err != nil:
		writeError(response, http.StatusBadRequest, err.Error())
	default:
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(response, status, record)
	}
}

func (s *Server) handleMatch(response http.ResponseWriter, request *http.Request) {
	var match trade.SignedMatch
	if err := decodeJSON(response, request, &match); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if match.Match.OrderID != request.PathValue("id") {
		writeError(response, http.StatusBadRequest, "path order ID does not match selection")
		return
	}
	if match.Match.Network != s.network {
		writeError(response, http.StatusBadRequest, fmt.Sprintf("relay accepts only %s trades", s.network))
		return
	}
	record, _, err := s.store.ConfirmMatch(request.PathValue("id"), match, s.now())
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(response, http.StatusNotFound, ErrNotFound.Error())
	case errors.Is(err, ErrNotOpen):
		writeError(response, http.StatusConflict, ErrNotOpen.Error())
	case err != nil:
		writeError(response, http.StatusBadRequest, err.Error())
	default:
		writeJSON(response, http.StatusOK, record)
	}
}

func (s *Server) handleMessage(response http.ResponseWriter, request *http.Request) {
	var message trade.SignedMessage
	if err := decodeJSON(response, request, &message); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if message.Message.Network != s.network {
		writeError(response, http.StatusBadRequest, fmt.Sprintf("relay accepts only %s messages", s.network))
		return
	}
	stored, created, err := s.store.PublishMessage(message, s.now())
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(response, http.StatusNotFound, ErrNotFound.Error())
	case errors.Is(err, ErrMessageSequence):
		writeError(response, http.StatusConflict, ErrMessageSequence.Error())
	case err != nil:
		writeError(response, http.StatusBadRequest, err.Error())
	default:
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(response, status, stored)
	}
}

func (s *Server) handleMailboxPoll(response http.ResponseWriter, request *http.Request) {
	var poll trade.SignedPoll
	if err := decodeJSON(response, request, &poll); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if poll.Poll.Network != s.network {
		writeError(response, http.StatusBadRequest, fmt.Sprintf("relay serves only %s mailboxes", s.network))
		return
	}
	page, err := s.store.PollMailbox(poll, s.now())
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(response, http.StatusOK, page)
}

func (s *Server) handleOrder(response http.ResponseWriter, request *http.Request) {
	record, err := s.store.Order(request.PathValue("id"), s.now())
	if errors.Is(err, ErrNotFound) {
		writeError(response, http.StatusNotFound, "order not found")
	} else if err != nil {
		s.internalError(response, err)
	} else {
		writeJSON(response, http.StatusOK, record)
	}
}

func (s *Server) handlePublish(response http.ResponseWriter, request *http.Request) {
	var signed order.Signed
	if err := decodeJSON(response, request, &signed); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if signed.Order.Network != s.network {
		writeError(response, http.StatusBadRequest, fmt.Sprintf("relay accepts only %s orders", s.network))
		return
	}
	record, created, err := s.store.Publish(signed, s.now())
	if errors.Is(err, ErrMakerLimit) {
		writeError(response, http.StatusTooManyRequests, ErrMakerLimit.Error())
		return
	} else if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(response, status, record)
}

func (s *Server) handleCancel(response http.ResponseWriter, request *http.Request) {
	var cancellation order.SignedCancellation
	if err := decodeJSON(response, request, &cancellation); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	if cancellation.Cancellation.OrderID != request.PathValue("id") {
		writeError(response, http.StatusBadRequest, "path order ID does not match cancellation")
		return
	}
	record, _, err := s.store.Cancel(cancellation, s.now())
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(response, http.StatusNotFound, ErrNotFound.Error())
	case errors.Is(err, ErrNotOpen):
		writeError(response, http.StatusConflict, ErrNotOpen.Error())
	case err != nil:
		writeError(response, http.StatusBadRequest, err.Error())
	default:
		writeJSON(response, http.StatusOK, record)
	}
}

func (s *Server) handleWeb(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requested := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
	if requested != "." && requested != "" && fs.ValidPath(requested) {
		if info, err := fs.Stat(embeddedWeb, "web/"+requested); err == nil && !info.IsDir() {
			if strings.Contains(requested, ".") {
				response.Header().Set("Cache-Control", "public, max-age=3600")
			}
			s.static.ServeHTTP(response, request)
			return
		}
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-cache")
	response.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = response.Write(s.index)
	}
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(response, request)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(response, request)
		if request.URL.Path == "/api/v1/orders" && request.Method == http.MethodPost {
			s.logger.Info("order publish request", "remote", request.RemoteAddr, "duration", time.Since(started))
		}
	})
}

func (s *Server) internalError(response http.ResponseWriter, err error) {
	s.logger.Error("relay request failed", "error", err)
	writeError(response, http.StatusInternalServerError, "internal server error")
}

func decodeJSON(response http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func positiveQueryInteger(raw string, fallback, maximum int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maximum {
		return 0, errors.New("integer is outside the allowed range")
	}
	return value, nil
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func writeError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, struct {
		Error string `json:"error"`
	}{message})
}

func init() {
	_ = mime.AddExtensionType(".webp", "image/webp")
}
