// Copyright 2018 Shift Devices AG
// Copyright 2020 Shift Crypto AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handlers

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/digitalbitbox/bitbox-wallet-app/backend"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/accounts"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/accounts/errors"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/banners"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/bitboxbase"
	baseHandlers "github.com/digitalbitbox/bitbox-wallet-app/backend/bitboxbase/handlers"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/coins/btc"
	accountHandlers "github.com/digitalbitbox/bitbox-wallet-app/backend/coins/btc/handlers"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/coins/coin"
	coinpkg "github.com/digitalbitbox/bitbox-wallet-app/backend/coins/coin"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/coins/eth"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/config"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/devices/bitbox"
	bitboxHandlers "github.com/digitalbitbox/bitbox-wallet-app/backend/devices/bitbox/handlers"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/devices/bitbox02"
	bitbox02Handlers "github.com/digitalbitbox/bitbox-wallet-app/backend/devices/bitbox02/handlers"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/devices/bitbox02bootloader"
	bitbox02bootloaderHandlers "github.com/digitalbitbox/bitbox-wallet-app/backend/devices/bitbox02bootloader/handlers"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/devices/device"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/keystore"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/rates"
	"github.com/digitalbitbox/bitbox-wallet-app/backend/signing"
	utilConfig "github.com/digitalbitbox/bitbox-wallet-app/util/config"
	"github.com/digitalbitbox/bitbox-wallet-app/util/errp"
	"github.com/digitalbitbox/bitbox-wallet-app/util/jsonp"
	"github.com/digitalbitbox/bitbox-wallet-app/util/locker"
	"github.com/digitalbitbox/bitbox-wallet-app/util/logging"
	"github.com/digitalbitbox/bitbox-wallet-app/util/observable"
	"github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	qrcode "github.com/skip2/go-qrcode"
)

// Backend models the API of the backend.
type Backend interface {
	observable.Interface

	Config() *config.Config
	DefaultAppConfig() config.AppConfig
	Coin(coinpkg.Code) (coinpkg.Coin, error)
	Testing() bool
	Accounts() []accounts.Interface
	Keystores() *keystore.Keystores
	CreateAndAddAccount(
		coin coinpkg.Coin,
		code string,
		name string,
		getSigningConfigurations func() (signing.Configurations, error),
		persist bool,
		emitEvent bool,
	) error
	OnAccountInit(f func(accounts.Interface))
	OnAccountUninit(f func(accounts.Interface))
	OnDeviceInit(f func(device.Interface))
	OnDeviceUninit(f func(deviceID string))
	DevicesRegistered() map[string]device.Interface
	OnBitBoxBaseInit(f func(*bitboxbase.BitBoxBase))
	OnBitBoxBaseUninit(f func(bitboxBaseID string))
	BitBoxBasesDetected() map[string]string
	BitBoxBasesRegistered() map[string]*bitboxbase.BitBoxBase
	Start() <-chan interface{}
	RegisterKeystore(keystore.Keystore)
	DeregisterKeystore()
	Register(device device.Interface) error
	Deregister(deviceID string)
	TryMakeNewBase(ip string) (bool, error)
	RatesUpdater() *rates.RateUpdater
	BitBoxBaseDeregister(bitboxBaseID string)
	DownloadCert(string) (string, error)
	CheckElectrumServer(*config.ServerInfo) error
	RegisterTestKeystore(string)
	NotifyUser(string)
	SystemOpen(string) error
	ReinitializeAccounts()
	CheckForUpdateIgnoringErrors() *backend.UpdateFile
	Banners() *banners.Banners
	Environment() backend.Environment
}

// Handlers provides a web api to the backend.
type Handlers struct {
	Router  *mux.Router
	backend Backend
	// apiData consists of the port on which this API will run and the authorization token, generated by the
	// backend to secure the API call. The data is fed into the static javascript app
	// that is served, so the client knows where and how to connect to.
	apiData           *ConnectionData
	backendEvents     chan interface{}
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
// If the port is -1 or the token is empty, we assume dev-mode.
func NewConnectionData(port int, token string) *ConnectionData {
	return &ConnectionData{
		port:    port,
		token:   token,
		devMode: len(token) == 0,
	}
}

func (connectionData *ConnectionData) isDev() bool {
	return connectionData.port == -1 || connectionData.token == ""
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	backend Backend,
	connData *ConnectionData,
) *Handlers {
	log := logging.Get().WithGroup("handlers")
	router := mux.NewRouter()
	handlers := &Handlers{
		Router:        router,
		backend:       backend,
		apiData:       connData,
		backendEvents: make(chan interface{}, 1000),
		websocketUpgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		log: logging.Get().WithGroup("handlers"),
	}

	getAPIRouter := func(subrouter *mux.Router) func(string, func(*http.Request) (interface{}, error)) *mux.Route {
		return func(path string, f func(*http.Request) (interface{}, error)) *mux.Route {
			return subrouter.Handle(path, ensureAPITokenValid(handlers.apiMiddleware(connData.isDev(), f),
				connData, log))
		}
	}

	apiRouter := router.PathPrefix("/api").Subrouter()
	getAPIRouter(apiRouter)("/qr", handlers.getQRCodeHandler).Methods("GET")
	getAPIRouter(apiRouter)("/config", handlers.getAppConfigHandler).Methods("GET")
	getAPIRouter(apiRouter)("/config/default", handlers.getDefaultConfigHandler).Methods("GET")
	getAPIRouter(apiRouter)("/config", handlers.postAppConfigHandler).Methods("POST")
	getAPIRouter(apiRouter)("/native-locale", handlers.getNativeLocaleHandler).Methods("GET")
	getAPIRouter(apiRouter)("/notify-user", handlers.postNotifyHandler).Methods("POST")
	getAPIRouter(apiRouter)("/open", handlers.postOpenHandler).Methods("POST")
	getAPIRouter(apiRouter)("/update", handlers.getUpdateHandler).Methods("GET")
	getAPIRouter(apiRouter)("/banners/{key}", handlers.getBannersHandler).Methods("GET")
	getAPIRouter(apiRouter)("/using-mobile-data", handlers.getUsingMobileDataHandler).Methods("GET")
	getAPIRouter(apiRouter)("/version", handlers.getVersionHandler).Methods("GET")
	getAPIRouter(apiRouter)("/testing", handlers.getTestingHandler).Methods("GET")
	getAPIRouter(apiRouter)("/account-add", handlers.postAddAccountHandler).Methods("POST")
	getAPIRouter(apiRouter)("/keystores", handlers.getKeystoresHandler).Methods("GET")
	getAPIRouter(apiRouter)("/accounts", handlers.getAccountsHandler).Methods("GET")
	getAPIRouter(apiRouter)("/accounts/reinitialize", handlers.postAccountsReinitializeHandler).Methods("POST")
	getAPIRouter(apiRouter)("/export-account-summary", handlers.postExportAccountSummary).Methods("POST")
	getAPIRouter(apiRouter)("/account-summary", handlers.getAccountSummary).Methods("GET")
	getAPIRouter(apiRouter)("/test/register", handlers.postRegisterTestKeystoreHandler).Methods("POST")
	getAPIRouter(apiRouter)("/test/deregister", handlers.postDeregisterTestKeystoreHandler).Methods("POST")
	getAPIRouter(apiRouter)("/rates", handlers.getRatesHandler).Methods("GET")
	getAPIRouter(apiRouter)("/coins/convertToFiat", handlers.getConvertToFiatHandler).Methods("GET")
	getAPIRouter(apiRouter)("/coins/convertFromFiat", handlers.getConvertFromFiatHandler).Methods("GET")
	getAPIRouter(apiRouter)("/coins/tltc/headers/status", handlers.getHeadersStatus(coinpkg.CodeTLTC)).Methods("GET")
	getAPIRouter(apiRouter)("/coins/tbtc/headers/status", handlers.getHeadersStatus(coinpkg.CodeTBTC)).Methods("GET")
	getAPIRouter(apiRouter)("/coins/ltc/headers/status", handlers.getHeadersStatus(coinpkg.CodeLTC)).Methods("GET")
	getAPIRouter(apiRouter)("/coins/btc/headers/status", handlers.getHeadersStatus(coinpkg.CodeBTC)).Methods("GET")
	getAPIRouter(apiRouter)("/certs/download", handlers.postCertsDownloadHandler).Methods("POST")
	getAPIRouter(apiRouter)("/electrum/check", handlers.postElectrumCheckHandler).Methods("POST")
	getAPIRouter(apiRouter)("/bitboxbases/establish-connection", handlers.postEstablishConnectionHandler).Methods("POST")

	devicesRouter := getAPIRouter(apiRouter.PathPrefix("/devices").Subrouter())
	devicesRouter("/registered", handlers.getDevicesRegisteredHandler).Methods("GET")

	bitboxBasesRouter := getAPIRouter(apiRouter.PathPrefix("/bitboxbases").Subrouter())
	bitboxBasesRouter("/registered", handlers.getBitBoxBasesRegisteredHandler).Methods("GET")
	bitboxBasesRouter("/detected", handlers.getBitBoxBasesDetectedHandler).Methods("GET")

	handlersMapLock := locker.Locker{}

	accountHandlersMap := map[string]*accountHandlers.Handlers{}
	getAccountHandlers := func(accountCode string) *accountHandlers.Handlers {
		defer handlersMapLock.Lock()()
		if _, ok := accountHandlersMap[accountCode]; !ok {
			accountHandlersMap[accountCode] = accountHandlers.NewHandlers(getAPIRouter(
				apiRouter.PathPrefix(fmt.Sprintf("/account/%s", accountCode)).Subrouter(),
			), log)
		}
		accHandlers := accountHandlersMap[accountCode]
		log.WithField("account-handlers", accHandlers).Debug("Account handlers")
		return accHandlers
	}

	backend.OnAccountInit(func(account accounts.Interface) {
		log.WithField("code", account.Config().Code).Debug("Initializing account")
		getAccountHandlers(account.Config().Code).Init(account)
	})
	backend.OnAccountUninit(func(account accounts.Interface) {
		getAccountHandlers(account.Config().Code).Uninit()
	})

	deviceHandlersMap := map[string]*bitboxHandlers.Handlers{}
	getDeviceHandlers := func(deviceID string) *bitboxHandlers.Handlers {
		defer handlersMapLock.Lock()()
		if _, ok := deviceHandlersMap[deviceID]; !ok {
			deviceHandlersMap[deviceID] = bitboxHandlers.NewHandlers(getAPIRouter(
				apiRouter.PathPrefix(fmt.Sprintf("/devices/%s", deviceID)).Subrouter(),
			), log)
		}
		return deviceHandlersMap[deviceID]
	}

	bitbox02HandlersMap := map[string]*bitbox02Handlers.Handlers{}
	getBitBox02Handlers := func(deviceID string) *bitbox02Handlers.Handlers {
		defer handlersMapLock.Lock()()
		if _, ok := bitbox02HandlersMap[deviceID]; !ok {
			bitbox02HandlersMap[deviceID] = bitbox02Handlers.NewHandlers(getAPIRouter(
				apiRouter.PathPrefix(fmt.Sprintf("/devices/bitbox02/%s", deviceID)).Subrouter(),
			), log)
		}
		return bitbox02HandlersMap[deviceID]
	}

	bitbox02BootloaderHandlersMap := map[string]*bitbox02bootloaderHandlers.Handlers{}
	getBitBox02BootloaderHandlers := func(deviceID string) *bitbox02bootloaderHandlers.Handlers {
		defer handlersMapLock.Lock()()
		if _, ok := bitbox02BootloaderHandlersMap[deviceID]; !ok {
			bitbox02BootloaderHandlersMap[deviceID] = bitbox02bootloaderHandlers.NewHandlers(getAPIRouter(
				apiRouter.PathPrefix(fmt.Sprintf("/devices/bitbox02-bootloader/%s", deviceID)).Subrouter(),
			), log)
		}
		return bitbox02BootloaderHandlersMap[deviceID]
	}

	baseHandlersMap := map[string]*baseHandlers.Handlers{}
	getBaseHandlers := func(bitboxBaseID string) *baseHandlers.Handlers {
		defer handlersMapLock.Lock()()
		if _, ok := baseHandlersMap[bitboxBaseID]; !ok {
			baseHandlersMap[bitboxBaseID] = baseHandlers.NewHandlers(getAPIRouter(
				apiRouter.PathPrefix(fmt.Sprintf("/bitboxbases/%s", bitboxBaseID)).Subrouter(),
			), log)
		}
		return baseHandlersMap[bitboxBaseID]
	}

	backend.OnDeviceInit(func(device device.Interface) {
		switch specificDevice := device.(type) {
		case *bitbox.Device:
			getDeviceHandlers(device.Identifier()).Init(specificDevice)
		case *bitbox02.Device:
			getBitBox02Handlers(device.Identifier()).Init(specificDevice)
		case *bitbox02bootloader.Device:
			getBitBox02BootloaderHandlers(device.Identifier()).Init(specificDevice)
		}
	})
	backend.OnDeviceUninit(func(deviceID string) {
		getDeviceHandlers(deviceID).Uninit()
	})

	backend.OnBitBoxBaseInit(func(baseDevice *bitboxbase.BitBoxBase) {
		getBaseHandlers(baseDevice.Identifier()).Init(baseDevice)
	})
	backend.OnBitBoxBaseUninit(func(bitboxBaseID string) {
		getBaseHandlers(bitboxBaseID).Uninit()
	})

	apiRouter.HandleFunc("/events", handlers.eventsHandler)

	// The backend relays events in two ways:
	// a) old school through the channel returned by Start()
	// b) new school via observable.
	// Merge both.
	events := backend.Start()
	go func() {
		for {
			handlers.backendEvents <- <-events
		}
	}()
	backend.Observe(func(event observable.Event) { handlers.backendEvents <- event })

	return handlers
}

// Events returns the push notifications channel.
func (handlers *Handlers) Events() <-chan interface{} {
	return handlers.backendEvents
}

func writeJSON(w io.Writer, value interface{}) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}

type accountJSON struct {
	CoinCode              coinpkg.Code `json:"coinCode"`
	CoinUnit              string       `json:"coinUnit"`
	Code                  string       `json:"code"`
	Name                  string       `json:"name"`
	BlockExplorerTxPrefix string       `json:"blockExplorerTxPrefix"`
}

func newAccountJSON(account accounts.Interface) *accountJSON {
	return &accountJSON{
		CoinCode:              account.Coin().Code(),
		CoinUnit:              account.Coin().Unit(false),
		Code:                  account.Config().Code,
		Name:                  account.Config().Name,
		BlockExplorerTxPrefix: account.Coin().BlockExplorerTransactionURLPrefix(),
	}
}

func (handlers *Handlers) getQRCodeHandler(r *http.Request) (interface{}, error) {
	data := r.URL.Query().Get("data")
	qr, err := qrcode.New(data, qrcode.Medium)
	if err != nil {
		return nil, errp.WithStack(err)
	}
	bytes, err := qr.PNG(256)
	if err != nil {
		return nil, errp.WithStack(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(bytes), nil
}

func (handlers *Handlers) getAppConfigHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.Config().AppConfig(), nil
}

func (handlers *Handlers) getDefaultConfigHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.DefaultAppConfig(), nil
}

func (handlers *Handlers) postAppConfigHandler(r *http.Request) (interface{}, error) {
	appConfig := config.AppConfig{}
	if err := json.NewDecoder(r.Body).Decode(&appConfig); err != nil {
		return nil, errp.WithStack(err)
	}
	return nil, handlers.backend.Config().SetAppConfig(appConfig)
}

// getNativeLocaleHandler returns user preferred UI language as reported
// by the native app layer.
// The response value may be invalid or unsupported by the app.
func (handlers *Handlers) getNativeLocaleHandler(*http.Request) (interface{}, error) {
	return handlers.backend.Environment().NativeLocale(), nil
}

func (handlers *Handlers) postNotifyHandler(r *http.Request) (interface{}, error) {
	payload := struct {
		Text string `json:"text"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return nil, errp.WithStack(err)
	}
	handlers.backend.NotifyUser(payload.Text)
	return nil, nil
}

func (handlers *Handlers) postOpenHandler(r *http.Request) (interface{}, error) {
	var url string
	if err := json.NewDecoder(r.Body).Decode(&url); err != nil {
		return nil, errp.WithStack(err)
	}
	return nil, handlers.backend.SystemOpen(url)
}

func (handlers *Handlers) getUpdateHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.CheckForUpdateIgnoringErrors(), nil
}

func (handlers *Handlers) getBannersHandler(r *http.Request) (interface{}, error) {
	return handlers.backend.Banners().GetMessage(banners.MessageKey(mux.Vars(r)["key"])), nil
}

func (handlers *Handlers) getUsingMobileDataHandler(r *http.Request) (interface{}, error) {
	return handlers.backend.Environment().UsingMobileData(), nil
}

func (handlers *Handlers) getVersionHandler(_ *http.Request) (interface{}, error) {
	return backend.Version.String(), nil
}

func (handlers *Handlers) getTestingHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.Testing(), nil
}

func (handlers *Handlers) postAddAccountHandler(r *http.Request) (interface{}, error) {
	jsonBody := map[string]string{}
	if err := json.NewDecoder(r.Body).Decode(&jsonBody); err != nil {
		return nil, errp.WithStack(err)
	}
	// The following parameters only work for watch-only singlesig accounts at the moment.
	jsonCoinCode := coinpkg.Code(jsonBody["coinCode"])
	jsonScriptType := jsonBody["scriptType"]
	jsonAccountName := jsonBody["accountName"]
	jsonExtendedPublicKey := jsonBody["extendedPublicKey"]
	jsonAddress := jsonBody["address"]

	coin, err := handlers.backend.Coin(jsonCoinCode)
	if err != nil {
		return nil, err
	}

	scriptType, err := signing.DecodeScriptType(jsonScriptType)
	if err != nil {
		return nil, err
	}
	keypath := signing.NewEmptyAbsoluteKeypath()

	var configuration *signing.Configuration
	var warningCode string

	if jsonAddress != "" {
		switch jsonCoinCode {
		case coinpkg.CodeBTC, coinpkg.CodeLTC, coinpkg.CodeTBTC, coinpkg.CodeTLTC:
			btcCoin, ok := coin.(*btc.Coin)
			if !ok {
				panic("unexpected type, expected: *btc.Coin")
			}
			_, err := btcCoin.DecodeAddress(jsonAddress)
			if err != nil {
				return map[string]interface{}{"success": false, "errorCode": "invalidAddress"}, nil
			}
			configuration = signing.NewAddressConfiguration(scriptType, keypath, jsonAddress)
		case coinpkg.CodeETH, coinpkg.CodeTETH:
			if !common.IsHexAddress(jsonAddress) {
				return map[string]interface{}{"success": false, "errorCode": "invalidAddress"}, nil
			}
			configuration = signing.NewAddressConfiguration(scriptType, keypath, jsonAddress)
		}

	} else {
		extendedPublicKey, err := hdkeychain.NewKeyFromString(jsonExtendedPublicKey)
		if err != nil {
			return map[string]interface{}{"success": false, "errorCode": "xpubInvalid"}, nil
		}
		if extendedPublicKey.IsPrivate() {
			return map[string]interface{}{"success": false, "errorCode": "xprivEntered"}, nil
		}
		if btcCoin, ok := coin.(*btc.Coin); ok {
			expectedNet := &chaincfg.Params{
				HDPublicKeyID: btc.XPubVersionForScriptType(btcCoin, scriptType),
			}
			if !extendedPublicKey.IsForNet(expectedNet) {
				warningCode = "xpubWrongNet"
			}
		}
		configuration = signing.NewSinglesigConfiguration(scriptType, keypath, extendedPublicKey)
	}

	getSigningConfigurations := func() (signing.Configurations, error) {
		return signing.Configurations{configuration}, nil
	}
	accountCode := fmt.Sprintf("%s-%s", configuration.Hash(), coin.Code())
	err = handlers.backend.CreateAndAddAccount(
		coin, accountCode, jsonAccountName, getSigningConfigurations, true, true)
	if errp.Cause(err) == backend.ErrAccountAlreadyExists {
		return map[string]interface{}{"success": false, "errorCode": "alreadyExists"}, nil
	}
	if err != nil {
		return map[string]interface{}{
			"success":      false,
			"errorCode":    "unknown",
			"errorMessage": err.Error(),
		}, nil
	}
	return map[string]interface{}{
		"success":     true,
		"accountCode": accountCode,
		"warningCode": warningCode,
	}, nil
}

func (handlers *Handlers) getKeystoresHandler(_ *http.Request) (interface{}, error) {
	type json struct {
		Type keystore.Type `json:"type"`
	}
	keystores := []*json{}
	for _, keystore := range handlers.backend.Keystores().Keystores() {
		keystores = append(keystores, &json{
			Type: keystore.Type(),
		})
	}
	return keystores, nil
}

func (handlers *Handlers) getAccountsHandler(_ *http.Request) (interface{}, error) {
	accounts := []*accountJSON{}
	for _, account := range handlers.backend.Accounts() {
		accounts = append(accounts, newAccountJSON(account))
	}
	return accounts, nil
}

func (handlers *Handlers) postAccountsReinitializeHandler(_ *http.Request) (interface{}, error) {
	handlers.backend.ReinitializeAccounts()
	return nil, nil
}

func (handlers *Handlers) getDevicesRegisteredHandler(_ *http.Request) (interface{}, error) {
	jsonDevices := map[string]string{}
	for deviceID, device := range handlers.backend.DevicesRegistered() {
		jsonDevices[deviceID] = device.ProductName()
	}
	return jsonDevices, nil
}

func (handlers *Handlers) getBitBoxBasesDetectedHandler(_ *http.Request) (interface{}, error) {
	jsonDetectedBases := map[string]string{}
	for hostname, baseIPv4 := range handlers.backend.BitBoxBasesDetected() {
		jsonDetectedBases[hostname] = baseIPv4
	}
	return jsonDetectedBases, nil
}

func (handlers *Handlers) getBitBoxBasesRegisteredHandler(_ *http.Request) (interface{}, error) {
	jsonRegisteredBases := map[string]string{}
	for bitboxBaseID, bitboxBase := range handlers.backend.BitBoxBasesRegistered() {
		jsonRegisteredBases[bitboxBaseID] = bitboxBase.GetLocalHostname()
	}
	return jsonRegisteredBases, nil
}

func (handlers *Handlers) postRegisterTestKeystoreHandler(r *http.Request) (interface{}, error) {
	if !handlers.backend.Testing() {
		return nil, errp.New("Test keystore not available")
	}
	jsonBody := map[string]string{}
	if err := json.NewDecoder(r.Body).Decode(&jsonBody); err != nil {
		return nil, errp.WithStack(err)
	}
	pin := jsonBody["pin"]
	handlers.backend.RegisterTestKeystore(pin)
	return nil, nil
}

func (handlers *Handlers) postDeregisterTestKeystoreHandler(_ *http.Request) (interface{}, error) {
	handlers.backend.DeregisterKeystore()
	return nil, nil
}

func (handlers *Handlers) getRatesHandler(_ *http.Request) (interface{}, error) {
	return handlers.backend.RatesUpdater().Last(), nil
}

func (handlers *Handlers) getConvertToFiatHandler(r *http.Request) (interface{}, error) {
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	amount := r.URL.Query().Get("amount")
	amountAsFloat, err := strconv.ParseFloat(amount, 64)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"errMsg":  "invalid amount",
		}, nil
	}
	rate := handlers.backend.RatesUpdater().Last()[from][to]
	return map[string]interface{}{
		"success":    true,
		"fiatAmount": strconv.FormatFloat(amountAsFloat*rate, 'f', 2, 64),
	}, nil
}

func (handlers *Handlers) getConvertFromFiatHandler(r *http.Request) (interface{}, error) {
	isFee := false
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	coin, err := handlers.backend.Coin(coinpkg.Code(to))
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"errMsg":  "internal error",
		}, nil
	}

	amount := r.URL.Query().Get("amount")
	amountAsFloat, err := strconv.ParseFloat(amount, 64)
	if err != nil {
		return map[string]interface{}{
			"success": false,
			"errMsg":  "invalid amount",
		}, nil
	}
	unit := coin.Unit(isFee)
	switch unit { // HACK: fake rates for testnet coins
	case "TBTC", "TLTC", "TETH", "RETH":
		unit = unit[1:]
	}
	rate := handlers.backend.RatesUpdater().Last()[unit][from]
	result := 0.0
	if rate != 0.0 {
		result = amountAsFloat / rate
	}
	return map[string]interface{}{
		"success": true,
		"amount":  strconv.FormatFloat(result, 'f', int(coin.Decimals(isFee)), 64),
	}, nil
}

func (handlers *Handlers) getHeadersStatus(coinCode coinpkg.Code) func(*http.Request) (interface{}, error) {
	return func(_ *http.Request) (interface{}, error) {
		coin, err := handlers.backend.Coin(coinCode)
		if err != nil {
			return nil, err
		}
		return coin.(*btc.Coin).Headers().Status()
	}
}

func (handlers *Handlers) postCertsDownloadHandler(r *http.Request) (interface{}, error) {
	var server string
	if err := json.NewDecoder(r.Body).Decode(&server); err != nil {
		return nil, errp.WithStack(err)
	}
	pemCert, err := handlers.backend.DownloadCert(server)
	if err != nil {
		return map[string]interface{}{
			"success":      false,
			"errorMessage": err.Error(),
		}, nil
	}
	return map[string]interface{}{
		"success": true,
		"pemCert": pemCert,
	}, nil
}

func (handlers *Handlers) postElectrumCheckHandler(r *http.Request) (interface{}, error) {
	var serverInfo config.ServerInfo
	if err := json.NewDecoder(r.Body).Decode(&serverInfo); err != nil {
		return nil, errp.WithStack(err)
	}

	if err := handlers.backend.CheckElectrumServer(&serverInfo); err != nil {
		return map[string]interface{}{
			"success":      false,
			"errorMessage": err.Error(),
		}, nil
	}
	return map[string]interface{}{
		"success": true,
	}, nil
}

func (handlers *Handlers) postEstablishConnectionHandler(r *http.Request) (interface{}, error) {
	jsonBody := map[string]string{}
	if err := json.NewDecoder(r.Body).Decode(&jsonBody); err != nil {
		return nil, errp.WithStack(err)
	}
	ip := jsonBody["ip"]
	handlers.log.WithField("ip", ip).Debug("Connect to middleware with the following ip:")

	success, err := handlers.backend.TryMakeNewBase(ip)
	if err != nil {
		return map[string]interface{}{
			"success":      success,
			"errorMessage": err.Error(),
		}, nil
	}
	return map[string]interface{}{
		"success": true,
	}, nil
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

// ensureAPITokenValid wraps the given handler with another handler function that calls isAPITokenValid().
func ensureAPITokenValid(h http.Handler, apiData *ConnectionData, log *logrus.Entry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPITokenValid(w, r, apiData, log) {
			h.ServeHTTP(w, r)
		}
	})
}

func (handlers *Handlers) apiMiddleware(devMode bool, h func(*http.Request) (interface{}, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			// recover from all panics and log error before panicking again
			if r := recover(); r != nil {
				handlers.log.WithField("panic", true).Errorf("%v\n%s", r, string(debug.Stack()))
				writeJSON(w, map[string]string{"error": fmt.Sprintf("%v", r)})
			}
		}()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if devMode {
			// This enables us to run a server on a different port serving just the UI, while still
			// allowing it to access the API.
			w.Header().Set("Access-Control-Allow-Origin", "http://localhost:8080")
		}
		value, err := h(r)
		if err != nil {
			handlers.log.WithError(err).Error("endpoint failed")
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, value)
	})
}

func (handlers *Handlers) formatAmountAsJSON(amount coin.Amount, coinInstance coinpkg.Coin, isFee bool) accountHandlers.FormattedAmount {
	return accountHandlers.FormattedAmount{
		Amount:      coinInstance.FormatAmount(amount, isFee),
		Unit:        coinInstance.Unit(isFee),
		Conversions: coin.Conversions(amount, coinInstance, isFee, handlers.backend.RatesUpdater()),
	}
}

func (handlers *Handlers) allCoinCodes() []string {
	allCoinCodes := []string{}
	for _, account := range handlers.backend.Accounts() {
		if account.FatalError() {
			continue
		}
		allCoinCodes = append(allCoinCodes, string(account.Coin().Code()))
	}
	return allCoinCodes
}

func (handlers *Handlers) getAccountSummary(_ *http.Request) (interface{}, error) {
	type chartEntry struct {
		Time  int64   `json:"time"`
		Value float64 `json:"value"`
	}

	type extendedAccountJSON struct {
		*accountJSON
		Balance map[string]interface{} `json:"balance"`
	}

	jsonAccounts := []*extendedAccountJSON{}
	totals := map[coinpkg.Coin]*big.Int{}
	// coin code to coin name.
	coinNames := map[string]string{}

	// If true, we are missing headers or historical conversion rates necessary to compute the chart
	// data,
	chartDataMissing := false

	// key: unix timestamp.
	chartEntriesDaily := map[int64]chartEntry{}
	chartEntriesHourly := map[int64]chartEntry{}

	fiat := handlers.backend.Config().AppConfig().Backend.MainFiat
	// Chart data until this point in time.
	until := handlers.backend.RatesUpdater().HistoryLatestTimestampAll(handlers.allCoinCodes(), fiat)
	if until.IsZero() || time.Since(until) > 2*time.Hour {
		chartDataMissing = true
		handlers.log.
			WithField("until", until).
			WithField("now", time.Now()).
			Info("ChartDataMissing")
	}
	for _, account := range handlers.backend.Accounts() {
		if account.FatalError() {
			continue
		}
		err := account.Initialize()
		if err != nil {
			return nil, err
		}
		balance, err := account.Balance()
		if err != nil {
			return nil, err
		}
		txs, err := account.Transactions()
		if err != nil {
			return nil, err
		}
		jsonAccounts = append(jsonAccounts, &extendedAccountJSON{
			accountJSON: newAccountJSON(account),
			Balance: map[string]interface{}{
				"available":   handlers.formatAmountAsJSON(balance.Available(), account.Coin(), false),
				"incoming":    handlers.formatAmountAsJSON(balance.Incoming(), account.Coin(), false),
				"hasIncoming": balance.Incoming().BigInt().Sign() > 0,
			},
		})

		_, ok := totals[account.Coin()]
		if !ok {
			totals[account.Coin()] = new(big.Int)
		}

		totals[account.Coin()] = new(big.Int).Add(totals[account.Coin()], balance.Available().BigInt())
		coinNames[string(account.Coin().Code())] = account.Coin().Name()

		// Below here, only chart data is being computed.
		if chartDataMissing {
			continue
		}

		// Time from which the chart turns from daily points to hourly points.
		hourlyFrom := time.Now().AddDate(0, 0, -7).Truncate(24 * time.Hour)

		earliestPriceAvailable := handlers.backend.RatesUpdater().HistoryEarliestTimestamp(
			string(account.Coin().Code()),
			fiat)
		earliestTxTime := txs.EarliestTime()
		if earliestTxTime.IsZero() {
			// Ignore the chart for this account, there is no timed transaction.
			continue
		}
		if earliestPriceAvailable.IsZero() || earliestTxTime.Before(earliestPriceAvailable) {
			chartDataMissing = true
			handlers.log.
				WithField("coin", account.Coin().Code()).
				WithField("earliestTxTime", earliestTxTime).
				WithField("earliestPriceAvailable", earliestPriceAvailable).
				Info("ChartDataMissing")
			continue
		}

		timeseriesDaily, err := txs.Timeseries(
			earliestTxTime.Truncate(24*time.Hour).Add(time.Hour),
			until,
			24*time.Hour,
		)
		if errp.Cause(err) == errors.ErrNotAvailable {
			handlers.log.WithField("coin", account.Coin().Code()).Info("ChartDataMissing")
			chartDataMissing = true
			continue
		}
		if err != nil {
			return nil, err
		}
		timeseriesHourly, err := txs.Timeseries(
			hourlyFrom,
			until,
			time.Hour,
		)
		if errp.Cause(err) == errors.ErrNotAvailable {
			handlers.log.WithField("coin", account.Coin().Code()).Info("ChartDataMissing")
			chartDataMissing = true
			continue
		}
		if err != nil {
			return nil, err
		}

		// e.g. 1e8 for Bitcoin/Litecoin, 1e18 for Ethereum, etc. Used to convert from the smallest
		// unit to the standard unit (BTC, LTC; ETH, etc.).
		coinDecimals := new(big.Int).Exp(
			big.NewInt(10),
			big.NewInt(int64(account.Coin().Decimals(false))),
			nil,
		)

		addChartData := func(coinCode coin.Code, timeseries []accounts.TimeseriesEntry, chartEntries map[int64]chartEntry) {
			for _, e := range timeseries {
				price := handlers.backend.RatesUpdater().PriceAt(
					string(coinCode),
					fiat,
					e.Time)
				timestamp := e.Time.Unix()
				chartEntry := chartEntries[timestamp]

				chartEntry.Time = timestamp
				fiatValue, _ := new(big.Rat).Mul(
					new(big.Rat).SetFrac(
						e.Value.BigInt(),
						coinDecimals,
					),
					new(big.Rat).SetFloat64(price),
				).Float64()
				chartEntry.Value += fiatValue
				chartEntries[timestamp] = chartEntry
			}
		}

		addChartData(account.Coin().Code(), timeseriesDaily, chartEntriesDaily)
		addChartData(account.Coin().Code(), timeseriesHourly, chartEntriesHourly)

		// HACK: We still use the latest prices from CryptoCompare for the account fiat balances
		// above (displayed in the summary table). Those might deviate from the latest historical
		// prices from coingecko, which results in different total balances in the chart and the
		// summary table.
		//
		// As a temporary workaround, until we use only one source for all prices, we manually add
		// one final datapoint reflecting the latest price. This can be removed once we stop using
		// CryptoCompare.
		now := time.Now().Unix()
		price, err := handlers.backend.RatesUpdater().LastForPair(string(account.Coin().Code()), fiat)
		if err != nil {
			chartDataMissing = true
			handlers.log.WithError(err).Info("ChartDataMissing")
			continue
		}
		fiatValue, _ := new(big.Rat).Mul(
			new(big.Rat).SetFrac(
				balance.Available().BigInt(),
				coinDecimals,
			),
			new(big.Rat).SetFloat64(price),
		).Float64()
		entry := chartEntriesHourly[now]
		entry.Time = now
		entry.Value += fiatValue
		chartEntriesHourly[now] = entry
	}

	jsonTotals := make(map[coinpkg.Code]accountHandlers.FormattedAmount)
	for c, total := range totals {
		jsonTotals[c.Code()] = handlers.formatAmountAsJSON(coin.NewAmount(total), c, false)
	}

	toSortedSlice := func(s map[int64]chartEntry) []chartEntry {
		result := make([]chartEntry, len(s))
		i := 0
		for _, entry := range s {
			result[i] = entry
			i++
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Time < result[j].Time })
		// Truncate leading zeroes.
		for i, e := range result {
			if e.Value != 0 {
				return result[i:]
			}
		}
		return result
	}

	return map[string]interface{}{
		"accounts":         jsonAccounts,
		"totals":           jsonTotals,
		"coinNames":        coinNames,
		"chartDataMissing": chartDataMissing,
		"chartDataDaily":   toSortedSlice(chartEntriesDaily),
		"chartDataHourly":  toSortedSlice(chartEntriesHourly),
		"chartFiat":        fiat,
	}, nil
}

func (handlers *Handlers) postExportAccountSummary(_ *http.Request) (interface{}, error) {
	name := time.Now().Format("2006-01-02-at-15-04-05-") + "Accounts-Summary.csv"
	downloadsDir, err := utilConfig.DownloadsDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(downloadsDir, name)
	handlers.log.Infof("Export account summary %s.", path)

	file, err := os.Create(path)
	if err != nil {
		return nil, errp.WithStack(err)
	}
	defer func() {
		err := file.Close()
		if err != nil {
			handlers.log.WithError(err).Error("Could not close the account summary file.")
		}
	}()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	err = writer.Write([]string{
		"Coin",
		"Name",
		"Balance",
		"Unit",
		"Type",
		"Xpubs",
		"Address",
	})
	if err != nil {
		return nil, errp.WithStack(err)
	}

	for _, account := range handlers.backend.Accounts() {
		if account.FatalError() {
			continue
		}
		err := account.Initialize()
		if err != nil {
			return nil, err
		}
		coin := account.Coin().Code()
		accountName := account.Config().Name
		balance, err := account.Balance()
		if err != nil {
			return nil, err
		}
		unit := account.Coin().SmallestUnit()
		var accountType string
		var xpubs []string
		var address string
		signingConfigurations := account.Info().SigningConfigurations
		if len(signingConfigurations) == 1 && signingConfigurations[0].IsAddressBased() {
			accountType = "address"
			address = signingConfigurations[0].Address()
		} else {
			accountType = "xpubs"
			for _, signingConfiguration := range signingConfigurations {
				if len(signingConfiguration.ExtendedPublicKeys()) != 1 {
					return nil, errp.New("multisig not supported in the export yet")
				}
				xpubs = append(xpubs, signingConfiguration.ExtendedPublicKeys()[0].String())
			}

			if _, ok := account.(*eth.Account); ok {
				address = signingConfigurations[0].Address()
			}
		}

		err = writer.Write([]string{
			string(coin),
			accountName,
			balance.Available().BigInt().String(),
			unit,
			accountType,
			strings.Join(xpubs, "; "),
			address,
		})
		if err != nil {
			return nil, errp.WithStack(err)
		}
	}
	return path, nil
}
