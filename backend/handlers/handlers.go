package handlers

// The following go:generate command compiles the static web assets into a Go package, so that they
// are built into the binary. The WEBASSETS env var must be set and point to the folder containing
// the web assets.

//go:generate echo $WEBASSETS
//go:generate go-bindata -pkg $GOPACKAGE -o assets.go -prefix $WEBASSETS $WEBASSETS

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/shiftdevices/godbb/util/errp"
	"github.com/shiftdevices/godbb/util/logging"
	"github.com/sirupsen/logrus"

	assetfs "github.com/elazarl/go-bindata-assetfs"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/shiftdevices/godbb/backend"
	"github.com/shiftdevices/godbb/backend/coins/btc"
	accountHandlers "github.com/shiftdevices/godbb/backend/coins/btc/handlers"
	"github.com/shiftdevices/godbb/backend/devices/bitbox"
	bitboxHandlers "github.com/shiftdevices/godbb/backend/devices/bitbox/handlers"
	"github.com/shiftdevices/godbb/backend/devices/device"
	"github.com/shiftdevices/godbb/backend/keystore/software"
	"github.com/shiftdevices/godbb/util/jsonp"
	qrcode "github.com/skip2/go-qrcode"
)

// Handlers provides a web api to the backend.
type Handlers struct {
	Router  *mux.Router
	backend backend.Interface
	// apiData consists of the port on which this API will run and the authorization token, generated by the
	// backend to secure the API call. The data is fed into the static javascript app
	// that is served, so the client knows where and how to connect to.
	apiData           *ConnectionData
	backendEvents     <-chan interface{}
	websocketUpgrader websocket.Upgrader
	log               *logrus.Entry
}

// ConnectionData contains the port and authorization token for communication with the backend.
type ConnectionData struct {
	port    int
	token   string
	devMode bool
}

// NewConnectionData creates a connection data struct which holds the port and token for the API.
// If the token is empty, we assume dev-mode.
func NewConnectionData(port int, token string) *ConnectionData {
	return &ConnectionData{
		port:    port,
		token:   token,
		devMode: len(token) == 0,
	}
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	theBackend backend.Interface,
	connData *ConnectionData,
) *Handlers {
	log := logging.Log.WithGroup("handlers")
	router := mux.NewRouter()

	handlers := &Handlers{
		Router:  router,
		backend: theBackend,
		apiData: connData,
		websocketUpgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		backendEvents: theBackend.Start(),
		log:           logging.Log.WithGroup("handlers"),
	}

	getAPIRouter := func(subrouter *mux.Router) func(string, func(*http.Request) (interface{}, error)) *mux.Route {
		return func(path string, f func(*http.Request) (interface{}, error)) *mux.Route {
			return subrouter.Handle(path, ensureAPITokenValid(apiMiddleware(f),
				connData, log))
		}
	}

	apiRouter := router.PathPrefix("/api").Subrouter()
	apiRouter.HandleFunc("/qr", handlers.getQRCodeHandler).Methods("GET")
	getAPIRouter(apiRouter)("/version", handlers.getVersionHandler).Methods("GET")
	getAPIRouter(apiRouter)("/testing", handlers.getTestingHandler).Methods("GET")
	getAPIRouter(apiRouter)("/wallets", handlers.getWalletsHandler).Methods("GET")
	getAPIRouter(apiRouter)("/wallet-status", handlers.getWalletStatusHandler).Methods("GET")
	getAPIRouter(apiRouter)("/test/register", handlers.registerTestKeyStoreHandler).Methods("POST")
	getAPIRouter(apiRouter)("/test/deregister", handlers.deregisterTestKeyStoreHandler).Methods("POST")

	devicesRouter := getAPIRouter(apiRouter.PathPrefix("/devices").Subrouter())
	devicesRouter("/registered", handlers.getDevicesRegisteredHandler).Methods("GET")

	theAccountHandlers := map[string]*accountHandlers.Handlers{}
	for _, accountCode := range []string{
		"btc", "btc-p2wpkh-p2sh", "btc-p2wpkh", "ltc-p2wpkh-p2sh",
		"tbtc", "tbtc-p2wpkh-p2sh", "tbtc-p2wpkh", "tltc-p2wpkh-p2sh",
		"rbtc", "rbtc-p2wpkh-p2sh",
	} {
		theAccountHandlers[accountCode] = accountHandlers.NewHandlers(getAPIRouter(
			apiRouter.PathPrefix(fmt.Sprintf("/wallet/%s", accountCode)).Subrouter()), log)
	}

	theBackend.OnWalletInit(func(account *btc.Account) {
		theAccountHandlers[account.Code()].Init(account)
	})
	theBackend.OnWalletUninit(func(account *btc.Account) {
		theAccountHandlers[account.Code()].Uninit()
	})

	deviceHandlersMap := map[string]*bitboxHandlers.Handlers{}
	getDeviceHandlers := func(deviceID string) *bitboxHandlers.Handlers {
		if _, ok := deviceHandlersMap[deviceID]; !ok {
			deviceHandlersMap[deviceID] = bitboxHandlers.NewHandlers(getAPIRouter(
				apiRouter.PathPrefix(fmt.Sprintf("/devices/%s", deviceID)).Subrouter(),
			), log)
		}
		return deviceHandlersMap[deviceID]
	}
	theBackend.OnDeviceInit(func(device device.Interface) {
		getDeviceHandlers(device.Identifier()).Init(device.(*bitbox.Device))
	})
	theBackend.OnDeviceUninit(func(deviceID string) {
		getDeviceHandlers(deviceID).Uninit()
	})

	apiRouter.HandleFunc("/events", handlers.eventsHandler)

	// Serve static files for the UI.
	router.Handle("/{rest:.*}",
		ensureAPITokenValid(
			ensureNoCacheForBundleJS(
				http.FileServer(&assetfs.AssetFS{
					Asset: func(name string) ([]byte, error) {
						body, err := Asset(name)
						if err != nil {
							err = errp.WithStack(err)
							return nil, err
						}
						if regexp.MustCompile(`^bundle.*\.js$`).MatchString(name) {
							body = handlers.interpolateConstants(body)

						}
						return body, nil
					},
					AssetDir:  AssetDir,
					AssetInfo: AssetInfo,
					Prefix:    "",
				})), connData, log))

	return handlers
}

func (handlers *Handlers) interpolateConstants(body []byte) []byte {
	for _, info := range []struct {
		key, value string
	}{
		{"API_PORT", fmt.Sprintf("%d", handlers.apiData.port)},
		{"API_TOKEN", fmt.Sprintf("%s", handlers.apiData.token)},
		{"LANG", handlers.backend.UserLanguage().String()},
	} {
		body = bytes.Replace(body, []byte(fmt.Sprintf("{{ %s }}", info.key)), []byte(info.value), -1)
	}
	return body
}

func writeJSON(w http.ResponseWriter, value interface{}) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}

func (handlers *Handlers) getVersionHandler(_ *http.Request) (interface{}, error) {
	return backend.Version.String(), nil
}

func (handlers *Handlers) getTestingHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.Testing(), nil
}

func (handlers *Handlers) getWalletsHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.Accounts(), nil
}

func (handlers *Handlers) getWalletStatusHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.WalletStatus(), nil
}

func (handlers *Handlers) getQRCodeHandler(w http.ResponseWriter, r *http.Request) {
	if isAPITokenValid(w, r, handlers.apiData, handlers.log) {
		data := r.URL.Query().Get("data")
		qr, err := qrcode.New(data, qrcode.Medium)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_ = qr.Write(256, w)
	}
}

func (handlers *Handlers) getDevicesRegisteredHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.DevicesRegistered(), nil
}

func (handlers *Handlers) registerTestKeyStoreHandler(r *http.Request) (interface{}, error) {
	jsonBody := map[string]string{}
	if err := json.NewDecoder(r.Body).Decode(&jsonBody); err != nil {
		return nil, errp.WithStack(err)
	}
	pin := jsonBody["pin"]
	softwareBasedKeystore := software.NewKeystoreFromPIN(
		handlers.backend.Keystores().Count(), pin)
	handlers.backend.RegisterKeystore(softwareBasedKeystore)
	return true, nil
}

func (handlers *Handlers) deregisterTestKeyStoreHandler(_ *http.Request) (interface{}, error) {
	handlers.backend.DeregisterKeystore()
	return true, nil
}

func (handlers *Handlers) eventsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := handlers.websocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		panic(err)
	}

	sendChan, quitChan := runWebsocket(conn, handlers.apiData, handlers.log)
	go func() {
		for {
			select {
			case <-quitChan:
				return
			default:
				select {
				case <-quitChan:
					return
				case event := <-handlers.backendEvents:
					sendChan <- jsonp.MustMarshal(event)
				}
			}
		}
	}()
}

// isAPITokenValid checks whether we are in dev or prod mode and, if we are in prod mode, verifies
// that an authorization token is received as an HTTP Authorization header and that it is valid.
func isAPITokenValid(w http.ResponseWriter, r *http.Request, apiData *ConnectionData, log *logrus.Entry) bool {
	methodLogEntry := log.WithField("path", r.URL.Path)
	// In dev mode, we allow unauthorized requests
	if apiData.devMode {
		// methodLogEntry.Debug("Allowing access without authorization token in dev mode")
		return true
	}
	methodLogEntry.Debug("Checking API token")

	if len(r.Header.Get("Authorization")) == 0 {
		methodLogEntry.Error("Missing token in API request. WARNING: this could be an attack on the API")
		http.Error(w, "missing token "+r.URL.Path, http.StatusUnauthorized)
		return false
	} else if len(r.Header.Get("Authorization")) != 0 && r.Header.Get("Authorization") != "Basic "+apiData.token {
		methodLogEntry.Error("Incorrect token in API request. WARNING: this could be an attack on the API")
		http.Error(w, "incorrect token", http.StatusUnauthorized)
		return false
	}
	return true
}

// ensureNoCacheForBundleJS adds the cache-control header to the HTTP response to
// prevent that the bundle.js is cached in the client and therefore not reloaded from
// the server, which is required to receive the current authorization token.
func ensureNoCacheForBundleJS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if regexp.MustCompile(`^\/bundle.*\.js$`).MatchString(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
		h.ServeHTTP(w, r)
	})
}

// ensureAPITokenValid wraps the given handler with another handler function that calls isAPITokenValid().
func ensureAPITokenValid(h http.Handler, apiData *ConnectionData, log *logrus.Entry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPITokenValid(w, r, apiData, log) {
			h.ServeHTTP(w, r)
		}
	})
}

func apiMiddleware(h func(*http.Request) (interface{}, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/json")
		// This enables us to run a server on a different port serving just the UI, while still
		// allowing it to access the API.
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:8080")
		value, err := h(r)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, value)
	})
}
