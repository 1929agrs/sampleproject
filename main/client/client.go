package main

import (
	"gofrugal/wstunnel/tunnel/client"
	"os"
	"gopkg.in/inconshreveable/log15.v2"
	"gopkg.in/ini.v1"
	"fmt"
	"io/ioutil"
	"encoding/json"
	"net/http"
	"strings"
	"github.com/marcsauter/single"
	"github.com/mattn/go-colorable"
	"gopkg.in/natefinch/lumberjack.v2"
	"gofrugal/wstunnel/tunnel/util"
	"io"
	"log"
	"time"
)

func main() {

	helpers.SetVV(VV)

	registerLogger(true, fmt.Sprintf("%s/gft_gateway.log", helpers.ExecutableFolder()), nil)
	log15.Info(fmt.Sprintf("app version : %s", helpers.VV))

	iniConfig := loadIniFile()

	// Check if an instance already running
	// Exit if this is duplicate
	s := single.New(fmt.Sprintf("gft_gateway_%s", iniConfig.Product))
	s.Lock()
	defer s.Unlock()

	tries := 120
	sleepTime := 1 * time.Minute
	var peerGroupResp *PeerGroupResp
	// Try to Register with peergroup until success / max try counter
	for try := 1; try <= tries; try++ {
		resp, pgError := registerToPeerGroup(iniConfig)
		if pgError != nil {
			log15.Error("peergroup registration error", "try", try, "error", pgError.Error())
			time.Sleep(sleepTime)
			continue
		}
		log15.Info("peergroup registration success", "try", try)
		peerGroupResp = resp
		break
	}
	if peerGroupResp == nil {
		log15.Error("failed to register with peergroup, exiting", "tries", tries)
		os.Exit(1)
	}

	tunnelClientArg := makeTunnelClientArg(iniConfig, peerGroupResp)

	err := client.NewWSTunnelClient(tunnelClientArg).Start()
	if err != nil {
		log15.Crit(err.Error())
		os.Exit(1)
	}
	<-make(chan struct{}, 0)
}

// Ini file value
type IniConfig struct {
	OrderNo             string `ini:"ORDERNO"`             // order number
	CustomerId          string `ini:"CUSTOMERID"`          // customer id
	Product             string `ini:"PRODUCT"`             // product
	ServerPath          string `ini:"SERVERPATH"`          // on-premise server url (eg: http://localhost:8482)
	PeerGroupServerPath string `ini:"PEERGROUPSERVERPATH"` // peer group server url
}

var IniFileName = "gft_gateway.ini"
var IniSection = "CONFIG"

// Load Ini File from executable path
func loadIniFile() *IniConfig {
	iniConfig := &IniConfig{}
	cfg, err := ini.Load(fmt.Sprintf("%s/%s", helpers.ExecutableFolder(), IniFileName))
	if err != nil {
		log15.Crit(fmt.Sprintf("error loading ini file : %s", err.Error()))
		os.Exit(1)
	}
	cfg.Section(IniSection).MapTo(iniConfig)
	return iniConfig
}

// Tunnel Registration request to PeerGroup
type TunnelRegisterReq struct {
	Identity            string `json:"identity"`            // identity (aka customerId)
	LicenseNo           string `json:"licenseNo"`           // license number
	TunnelClientVersion string `json:"tunnelClientVersion"` // TODO: tunnelClientVersion
}

// Tunnel Registration response from PeerGroup
type PeerGroupResp struct {
	IsTunnelEnabled          bool   `json:"isTunnelEnabled"`          // is tunnel enabled ?
	IsTunnelServerSSLEnabled bool   `json:"isTunnelServerSSLEnabled"` // is tunnel server SSL enabled ?
	TunnelServerHost         string `json:"tunnelServerHost"`         // tunnel server host url
	TunnelServerToken        string `json:"tunnelServerToken"`        // token for tunnel server
}

// Get ws tunnel server url based on ssl config
func (pgr *PeerGroupResp) getTunnelServerUrl() string {
	protocol := "ws://"
	if pgr.IsTunnelServerSSLEnabled {
		protocol = "wss://"
	}
	return protocol + pgr.TunnelServerHost
}

var ContentType = "application/json"

// Register Tunnel client startup with PeerGroup
// Proceed or exit based on response
func registerToPeerGroup(iniConfig *IniConfig) (*PeerGroupResp, error) {

	tunnelClientReq := &TunnelRegisterReq{iniConfig.CustomerId, iniConfig.OrderNo, ""}
	postJsonByte, err := json.Marshal(tunnelClientReq)
	if err != nil {
		return nil, fmt.Errorf("error creating json for peer group request : %s", err.Error())
	}
	postJson := string(postJsonByte)

	pgTunnelRegisterUrl := iniConfig.PeerGroupServerPath + "/PeerGroupDNS/RegisterTunnel"
	resp, err := http.Post(pgTunnelRegisterUrl, ContentType, strings.NewReader(postJson))

	if err != nil {
		return nil, fmt.Errorf("error in peer group post request : %s", err.Error())
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("error reading peer group error response : %s", err.Error())
		}
		return nil, fmt.Errorf("error in peer group response : %s", string(body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading peer group response body : %s", err.Error())
	}
	peerGroupResp := new(PeerGroupResp)
	err = json.Unmarshal(body, &peerGroupResp)
	if err != nil {
		return nil, fmt.Errorf("error reading (unmarshalling) peer group response body : %s", err.Error())
	}
	if !peerGroupResp.IsTunnelEnabled {
		return nil, fmt.Errorf("tunnelling not enabled in peer group, shutting down tunnel client..!!")
	}

	return peerGroupResp, nil
}

// Form Tunnel Client Arg
func makeTunnelClientArg(iniConfig *IniConfig, peerGroupResp *PeerGroupResp) *client.TunnelClientArg {
	return &client.TunnelClientArg{
		Token:      peerGroupResp.TunnelServerToken,
		OrderNo:    iniConfig.OrderNo,
		TunnelUrl:  peerGroupResp.getTunnelServerUrl(),
		ServerPath: iniConfig.ServerPath,
	}
}

// Register logging configuration
func registerLogger(terminalLog bool, plainLogFile string, lvl *log15.Lvl) (io.Writer) {
	handlers := []log15.Handler{}

	// Terminal Log
	if terminalLog {
		handlers = append(handlers, log15.StreamHandler(colorable.NewColorableStdout(), log15.TerminalFormat()))
	}

	// Plain Log file Handler
	rotatingPlainLogger := &lumberjack.Logger{
		Filename:  plainLogFile,
		MaxSize:   10,
		Compress:  true,
		LocalTime: true,
	}
	// Calculate Max Backups
	{
		fixedSize := 200
		quo := fixedSize / rotatingPlainLogger.MaxSize
		rem := fixedSize % rotatingPlainLogger.MaxSize
		maxBackups := quo
		if rem > 0 {
			maxBackups += 1
		}
		rotatingPlainLogger.MaxBackups = maxBackups
	}
	{
		// Redirect core logs to logger file
		log.SetOutput(rotatingPlainLogger)
	}
	plainLogHdr := log15.StreamHandler(rotatingPlainLogger, log15.LogfmtFormat())
	if lvl != nil {
		filterLvl, _ := log15.LvlFromString(lvl.String())
		plainLogHdr = log15.LvlFilterHandler(filterLvl, plainLogHdr)
	}
	handlers = append(handlers, plainLogHdr)

	multiHandler := log15.MultiHandler(handlers...)
	log15.Root().SetHandler(multiHandler)

	return rotatingPlainLogger
}
