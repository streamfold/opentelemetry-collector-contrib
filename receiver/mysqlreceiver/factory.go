// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package mysqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/mysqlreceiver"

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scraperhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/mysqlreceiver/internal/metadata"
)

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		metadata.Type,
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, metadata.MetricsStability),
		receiver.WithLogs(createLogsReceiver, metadata.LogsStability),
	)
}

func createDefaultConfig() component.Config {
	cfg := scraperhelper.NewDefaultControllerConfig()
	cfg.CollectionInterval = 10 * time.Second
	return &Config{
		ControllerConfig:     cfg,
		AllowNativePasswords: true,
		Username:             "root",
		AddrConfig: confignet.AddrConfig{
			Endpoint:  "localhost:3306",
			Transport: confignet.TransportTypeTCP,
		},
		MetricsBuilderConfig: metadata.DefaultMetricsBuilderConfig(),
		StatementEvents: StatementEventsConfig{
			DigestTextLimit: defaultStatementEventsDigestTextLimit,
			Limit:           defaultStatementEventsLimit,
			TimeLimit:       defaultStatementEventsTimeLimit,
		},
		QuerySampleCollection: QuerySampleCollection{
			MaxRowsPerQuery: 100,
		},
	}
}

func createMetricsReceiver(
	_ context.Context,
	params receiver.Settings,
	rConf component.Config,
	consumer consumer.Metrics,
) (receiver.Metrics, error) {
	cfg := rConf.(*Config)

	ns := newMySQLScraper(params, cfg)
	s, err := scraper.NewMetrics(ns.scrape, scraper.WithStart(ns.start),
		scraper.WithShutdown(ns.shutdown))
	if err != nil {
		return nil, err
	}

	return scraperhelper.NewMetricsController(
		&cfg.ControllerConfig, params, consumer,
		scraperhelper.AddScraper(metadata.Type, s),
	)
}

func createLogsReceiver(
	_ context.Context,
	params receiver.Settings,
	rConf component.Config,
	consumer consumer.Logs,
) (receiver.Logs, error) {
	cfg := rConf.(*Config)

	ns := newMySQLScraper(params, cfg)
	s, err := scraper.NewLogs(ns.scrapeLog, scraper.WithStart(ns.start),
		scraper.WithShutdown(ns.shutdown))
	if err != nil {
		return nil, err
	}

	opts := make([]scraperhelper.ControllerOption, 0)
	opt := scraperhelper.AddFactoryWithConfig(
		scraper.NewFactory(metadata.Type, nil,
			scraper.WithLogs(func(context.Context, scraper.Settings, component.Config) (scraper.Logs, error) {
				return s, nil
			}, component.StabilityLevelAlpha)), nil)
	opts = append(opts, opt)
	return scraperhelper.NewLogsController(
		&cfg.ControllerConfig, params, consumer,
		opts...,
	)
}
