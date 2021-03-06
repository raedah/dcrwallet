// Copyright (c) 2016 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/decred/dcrrpcclient"
	"github.com/decred/dcrwallet/ticketbuyer"
	"github.com/decred/dcrwallet/wallet"
	flags "github.com/jessevdk/go-flags"
)

const (
	defaultAccountName               = "default"
	defaultMaxFee                    = 1.0
	defaultMinFee                    = 0.01
	defaultMaxPriceScale             = 2.0
	defaultMinPriceScale             = 0.7
	defaultAvgVWAPPriceDelta         = 2880
	defaultMaxPerBlock               = 20
	defaultBlocksToAvg               = 11
	defaultFeeTargetScaling          = 1.05
	defaultFeeSource                 = ticketbuyer.TicketFeeMean
	defaultTxFee                     = 0.01
	defaultMaxPriceAbsolute          = -1
	defaultAvgPriceMode              = ticketbuyer.PriceTargetVWAP
	defaultTicketBuyerConfigFilename = "ticketbuyer.conf"
)

type ticketBuyerConfig struct {
	ConfigFile        string  `short:"C" long:"configfile" description:"Path to configuration file"`
	MaxPriceScale     float64 `long:"maxpricescale" description:"Attempt to prevent the stake difficulty from going above this multiplier (>1.0) by manipulation (default: 2.0, 0.0 to disable)"`
	MinPriceScale     float64 `long:"minpricescale" description:"Attempt to prevent the stake difficulty from going below this multiplier (<1.0) by manipulation (default: 0.7, 0.0 to disable)"`
	PriceTarget       float64 `long:"pricetarget" description:"A target to try to seek setting the stake price to rather than meeting the average price (default: 0.0, 0.0 to disable)"`
	AvgPriceMode      string  `long:"avgpricemode" description:"The mode to use for calculating the average price if pricetarget is disabled (default: dual)"`
	AvgPriceVWAPDelta int     `long:"avgpricevwapdelta" description:"The number of blocks to use from the current block to calculate the VWAP (default: 2880)"`
	MaxFee            float64 `long:"maxfee" description:"Maximum ticket fee per KB (default: 1.0 Coin/KB)"`
	MinFee            float64 `long:"minfee" description:"Minimum ticket fee per KB (default: 0.01 Coin/KB)"`
	FeeSource         string  `long:"feesource" description:"The fee source to use for ticket fee per KB (median or mean, default: mean)"`
	MaxPerBlock       int     `long:"maxperblock" description:"Maximum tickets per block, with negative numbers indicating buy one ticket every 1-in-n blocks (default: 20)"`
	BlocksToAvg       int     `long:"blockstoavg" description:"Number of blocks to average for fees calculation (default: 11)"`
	FeeTargetScaling  float64 `long:"feetargetscaling" description:"The amount above the mean fee in the previous blocks to purchase tickets with, proportional e.g. 1.05 = 105% (default: 1.05)"`
	BuyImmediately    bool    `long:"buyimmediately" description:"Buy tickets immediately. The overrides the default behavior of spreading purchases evenly throughout window"`

	// Deprecated options for migrating from dcrticketbuyer
	DataDir          string `short:"b" long:"datadir" description:"Directory to store data"`
	ShowVersion      bool   `short:"V" long:"version" description:"Display version information and exit"`
	TestNet          bool   `long:"testnet" description:"Use the test network (default mainnet)"`
	SimNet           bool   `long:"simnet" description:"Use the simulation test network (default mainnet)"`
	DebugLevel       string `short:"d" long:"debuglevel" description:"Logging level {trace, debug, info, warn, error, critical}"`
	LogDir           string `long:"logdir" description:"Directory to log output"`
	HTTPSvrBind      string `long:"httpsvrbind" description:"IP to bind for the HTTP server that tracks ticket purchase metrics (default: \"\" or localhost)"`
	HTTPSvrPort      int    `long:"httpsvrport" description:"Server port for the HTTP server that tracks ticket purchase metrics; disabled if 0 (default: 0)"`
	HTTPUIPath       string `long:"httpuipath" description:"Deprecated and unused option for backwards compatibility."`
	DcrdUser         string `long:"dcrduser" description:"Daemon RPC user name"`
	DcrdPass         string `long:"dcrdpass" description:"Daemon RPC password"`
	DcrdServ         string `long:"dcrdserv" description:"Hostname/IP and port of dcrd RPC server to connect to (default localhost:9109, testnet: localhost:19109, simnet: localhost:19556)"`
	DcrdCert         string `long:"dcrdcert" description:"File containing the dcrd certificate file"`
	DcrwUser         string `long:"dcrwuser" description:"Wallet RPC user name"`
	DcrwPass         string `long:"dcrwpass" description:"Wallet RPC password"`
	DcrwServ         string `long:"dcrwserv" description:"Hostname/IP and port of dcrwallet RPC server to connect to (default localhost:9110, testnet: localhost:19110, simnet: localhost:19557)"`
	DcrwCert         string `long:"dcrwcert" description:"File containing the dcrwallet certificate file"`
	DisableClientTLS bool   `long:"noclienttls" description:"Disable TLS for the RPC client -- NOTE: This is only allowed if the RPC client is connecting to localhost"`

	// Deprecated options which have aliases or counterparts in dcrwallet
	AccountName       string  `long:"accountname" description:"Name of the account to buy tickets from (default: default)"`
	TicketAddress     string  `long:"ticketaddress" description:"Address to give ticket voting rights to"`
	PoolAddress       string  `long:"pooladdress" description:"Address to give pool fees rights to"`
	PoolFees          float64 `long:"poolfees" description:"The pool fee base rate for a given pool as a percentage (0.01 to 100.00%)"`
	MaxPriceAbsolute  float64 `long:"maxpriceabsolute" description:"The absolute maximum price to pay for a ticket. (example: 100.0 Coin) The default is 25% above the average price."`
	TxFee             float64 `long:"txfee" description:"Default regular tx fee per KB, for consolidations (default: 0.01 Coin/KB)"`
	BalanceToMaintain float64 `long:"balancetomaintain" description:"Balance to try to maintain in the wallet"`
}

func newTicketBuyerConfig(appConfig *config, parsedConfig *ticketBuyerConfig) *ticketbuyer.Config {
	return &ticketbuyer.Config{
		AccountName:       appConfig.PurchaseAccount,
		AvgPriceMode:      parsedConfig.AvgPriceMode,
		AvgPriceVWAPDelta: parsedConfig.AvgPriceVWAPDelta,
		BalanceToMaintain: appConfig.BalanceToMaintain,
		BlocksToAvg:       parsedConfig.BlocksToAvg,
		BuyImmediately:    parsedConfig.BuyImmediately,
		FeeSource:         parsedConfig.FeeSource,
		FeeTargetScaling:  parsedConfig.FeeTargetScaling,
		MinFee:            parsedConfig.MinFee,
		MinPriceScale:     parsedConfig.MinPriceScale,
		MaxFee:            parsedConfig.MaxFee,
		MaxPerBlock:       parsedConfig.MaxPerBlock,
		MaxPriceAbsolute:  parsedConfig.MaxPriceAbsolute,
		MaxPriceScale:     parsedConfig.MaxPriceScale,
		PoolAddress:       appConfig.PoolAddress,
		PoolFees:          appConfig.PoolFees,
		PriceTarget:       parsedConfig.PriceTarget,
		TicketAddress:     appConfig.TicketAddress,
		TxFee:             parsedConfig.TxFee,
	}
}

// loadTicketBuyerConfig initializes and parses the config using a config file.
func loadTicketBuyerConfig(appConfig *config) (*ticketbuyer.Config, error) {
	defaultTicketBuyerConfigFile := filepath.Join(appConfig.AppDataDir,
		defaultTicketBuyerConfigFilename)
	// Default config.
	cfg := ticketBuyerConfig{
		ConfigFile:        defaultTicketBuyerConfigFile,
		MinPriceScale:     defaultMinPriceScale,
		MaxPriceScale:     defaultMaxPriceScale,
		AvgPriceMode:      defaultAvgPriceMode,
		AvgPriceVWAPDelta: defaultAvgVWAPPriceDelta,
		MaxFee:            defaultMaxFee,
		MinFee:            defaultMinFee,
		FeeSource:         defaultFeeSource,
		MaxPerBlock:       defaultMaxPerBlock,
		BlocksToAvg:       defaultBlocksToAvg,
		FeeTargetScaling:  defaultFeeTargetScaling,

		AccountName:      defaultAccountName,
		MaxPriceAbsolute: defaultMaxPriceAbsolute,
		TxFee:            defaultTxFee,
	}

	if _, err := os.Stat(defaultTicketBuyerConfigFile); os.IsNotExist(err) {
		return newTicketBuyerConfig(appConfig, &cfg), nil
	}

	// Load additional config from file.
	var configFileError error
	parser := flags.NewParser(&cfg, flags.Default)
	err := flags.NewIniParser(parser).ParseFile(cfg.ConfigFile)
	if err != nil {
		if _, ok := err.(*os.PathError); !ok {
			log.Warn(err)
			parser.WriteHelp(os.Stderr)
			return nil, err
		}
		configFileError = err
	}

	if configFileError != nil {
		log.Warnf("%v", configFileError)
	}

	// Check deprecated aliases.
	if cfg.AccountName != defaultAccountName {
		log.Warn("accountname option has been replaced by " +
			"wallet option purchaseaccount -- please update your config")
	}

	// Make sure the fee source type given is valid.
	switch cfg.FeeSource {
	case ticketbuyer.TicketFeeMean:
	case ticketbuyer.TicketFeeMedian:
	default:
		str := "%s: Invalid fee source '%s'"
		err := fmt.Errorf(str, "loadTicketBuyerConfig", cfg.FeeSource)
		log.Warnf(err.Error())
		return nil, err
	}

	// Make sure a valid average price mode is given.
	switch cfg.AvgPriceMode {
	case ticketbuyer.PriceTargetVWAP:
	case ticketbuyer.PriceTargetPool:
	case ticketbuyer.PriceTargetDual:
	default:
		str := "%s: Invalid average price mode '%s'"
		err := fmt.Errorf(str, "loadTicketBuyerConfig", cfg.AvgPriceMode)
		log.Warnf(err.Error())
		return nil, err
	}

	return newTicketBuyerConfig(appConfig, &cfg), nil
}

// startTicketPurchase launches ticketbuyer to start purchasing tickets.
func startTicketPurchase(w *wallet.Wallet, dcrdClient *dcrrpcclient.Client,
	passphrase []byte, ticketbuyerCfg *ticketbuyer.Config) {

	tkbyLog.Infof("Starting ticket buyer")

	p, err := ticketbuyer.NewTicketPurchaser(ticketbuyerCfg,
		dcrdClient, w, activeNet.Params)
	if err != nil {
		tkbyLog.Errorf("Error starting ticketbuyer: %v", err)
		return
	}
	if passphrase != nil {
		var unlockAfter <-chan time.Time
		err = w.Unlock(passphrase, unlockAfter)
		if err != nil {
			tkbyLog.Errorf("Error unlocking wallet: %v", err)
			return
		}
	}
	quit := make(chan struct{})
	n := w.NtfnServer.MainTipChangedNotifications()
	pm := ticketbuyer.NewPurchaseManager(w, p, n.C, quit)
	go pm.NotificationHandler()
	go func() {
		dcrdClient.WaitForShutdown()

		tkbyLog.Infof("Stopping ticket buyer")
		n.Done()
		close(quit)
	}()
}
