// Package localui serves the browser interface on loopback. A random bootstrap
// URL installs a SameSite session cookie, so another website cannot drive the
// local wallet API through DNS rebinding or cross-site form submissions.
package localui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/petoshi/qday-swap/internal/app"
	"github.com/petoshi/qday-swap/internal/relay"
	"github.com/petoshi/qday-swap/internal/swapstate"
	"github.com/petoshi/qday-swap/internal/walletd"
)

const (
	maximumBody = 64 << 10
	sessionName = "qday_swap_session"
)

//go:embed web
var assets embed.FS

type Application interface {
	State(context.Context) app.State
	Setup(context.Context, string, string) (app.SetupResult, error)
	Unlock(context.Context, string) error
	Lock(context.Context) error
	RecoveryPhrase() (string, error)
	QDAYReceiveAddress(context.Context) (walletd.Address, error)
	BitcoinReceiveAddress() (string, error)
	Orders(context.Context, string, int) (relay.ResultPage, error)
	MarketPrice(context.Context) (relay.MarketPrice, error)
	QuoteOffer(context.Context, app.QuoteOfferRequest) (app.OfferQuote, error)
	CreateOffer(context.Context, app.CreateOfferRequest) (relay.Record, error)
	CancelOffer(context.Context, string) (relay.Record, error)
	AcceptOffer(context.Context, string) (swapstate.Negotiation, error)
	MatchAcceptance(context.Context, string) (swapstate.Swap, error)
	Negotiations() (app.Negotiations, error)
}

type Server struct {
	application Application
	host        string
	origin      string
	bootstrap   string
	session     string
	static      http.Handler
}

func New(application Application, host string) (*Server, error) {
	if application == nil {
		return nil, errors.New("local application is required")
	} else if host == "" || strings.ContainsAny(host, "/\\") {
		return nil, errors.New("local UI host is invalid")
	}
	web, err := fs.Sub(assets, "web")
	if err != nil {
		return nil, err
	}
	bootstrap, err := randomToken()
	if err != nil {
		return nil, err
	}
	session, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &Server{
		application: application, host: host, origin: "http://" + host,
		bootstrap: bootstrap, session: session, static: http.FileServer(http.FS(web)),
	}, nil
}

func randomToken() (string, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(entropy[:]), nil
}

func (s *Server) OpenURL() string { return s.origin + "/open/" + s.bootstrap }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)
	if r.Host != s.host {
		writeError(w, http.StatusMisdirectedRequest, "invalid local host")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/open/"+s.bootstrap {
		http.SetCookie(w, &http.Cookie{
			Name: sessionName, Value: s.session, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, MaxAge: 0,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "open QDAY Swap from the application window")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Origin") != s.origin {
			writeError(w, http.StatusForbidden, "invalid request origin")
			return
		}
		s.serveAPI(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/index.html" || strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == "/app.js" || r.URL.Path == "/styles.css" || r.URL.Path == "/favicon.svg" {
		s.static.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) authorized(r *http.Request) bool {
	cookie, err := r.Cookie(sessionName)
	return err == nil && len(cookie.Value) == len(s.session) && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.session)) == 1
}

func (s *Server) securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/state":
		writeJSON(w, http.StatusOK, s.application.State(r.Context()))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/setup":
		var request struct {
			Password string `json:"password"`
			Phrase   string `json:"phrase,omitempty"`
		}
		if !decode(w, r, &request) {
			return
		}
		result, err := s.application.Setup(r.Context(), request.Password, request.Phrase)
		request.Password, request.Phrase = "", ""
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, result)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/unlock":
		var request struct {
			Password string `json:"password"`
		}
		if !decode(w, r, &request) {
			return
		}
		err := s.application.Unlock(r.Context(), request.Password)
		request.Password = ""
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.application.State(r.Context()))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/lock":
		var request struct{}
		if !decode(w, r, &request) {
			return
		}
		if err := s.application.Lock(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.application.State(r.Context()))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/recovery":
		var request struct{}
		if !decode(w, r, &request) {
			return
		}
		phrase, err := s.application.RecoveryPhrase()
		if err != nil {
			writeError(w, http.StatusLocked, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"phrase": phrase})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/qday/address":
		address, err := s.application.QDAYReceiveAddress(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, address)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/bitcoin/address":
		address, err := s.application.BitcoinReceiveAddress()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"address": address})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/orders":
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			var err error
			page, err = strconv.Atoi(raw)
			if err != nil || page < 1 {
				writeError(w, http.StatusBadRequest, "invalid page")
				return
			}
		}
		orders, err := s.application.Orders(r.Context(), r.URL.Query().Get("giveAsset"), page)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, orders)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/price":
		price, err := s.application.MarketPrice(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, price)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/offers/quote":
		var request app.QuoteOfferRequest
		if !decode(w, r, &request) {
			return
		}
		quote, err := s.application.QuoteOffer(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, quote)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/orders":
		var request app.CreateOfferRequest
		if !decode(w, r, &request) {
			return
		}
		record, err := s.application.CreateOffer(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, record)
	case r.Method == http.MethodPost && routeID(r.URL.Path, "/api/v1/orders/", "/cancel") != "":
		var request struct{}
		if !decode(w, r, &request) {
			return
		}
		record, err := s.application.CancelOffer(r.Context(), routeID(r.URL.Path, "/api/v1/orders/", "/cancel"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, record)
	case r.Method == http.MethodPost && routeID(r.URL.Path, "/api/v1/orders/", "/accept") != "":
		var request struct{}
		if !decode(w, r, &request) {
			return
		}
		negotiation, err := s.application.AcceptOffer(r.Context(), routeID(r.URL.Path, "/api/v1/orders/", "/accept"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, negotiation)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/negotiations":
		negotiations, err := s.application.Negotiations()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, negotiations)
	case r.Method == http.MethodPost && routeID(r.URL.Path, "/api/v1/acceptances/", "/match") != "":
		var request struct{}
		if !decode(w, r, &request) {
			return
		}
		swap, err := s.application.MatchAcceptance(r.Context(), routeID(r.URL.Path, "/api/v1/acceptances/", "/match"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, swap)
	default:
		writeError(w, http.StatusNotFound, "API endpoint not found")
	}
}

func routeID(path, prefix, suffix string) string {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximumBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	} else if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON value")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
