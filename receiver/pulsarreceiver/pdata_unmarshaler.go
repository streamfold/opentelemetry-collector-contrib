// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package pulsarreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/pulsarreceiver"

import (
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// copy from kafka receiver
type pdataLogsUnmarshaler struct {
	plog.Unmarshaler
	encoding string
}

func (p pdataLogsUnmarshaler) Unmarshal(buf []byte) (plog.Logs, error) {
	return p.UnmarshalLogs(buf)
}

func (p pdataLogsUnmarshaler) Encoding() string {
	return p.encoding
}

func newPdataLogsUnmarshaler(unmarshaler plog.Unmarshaler, encoding string) LogsUnmarshaler {
	return pdataLogsUnmarshaler{
		Unmarshaler: unmarshaler,
		encoding:    encoding,
	}
}

type pdataTracesUnmarshaler struct {
	ptrace.Unmarshaler
	encoding string
}

func (p pdataTracesUnmarshaler) Unmarshal(buf []byte) (ptrace.Traces, error) {
	return p.UnmarshalTraces(buf)
}

func (p pdataTracesUnmarshaler) Encoding() string {
	return p.encoding
}

func newPdataTracesUnmarshaler(unmarshaler ptrace.Unmarshaler, encoding string) TracesUnmarshaler {
	return pdataTracesUnmarshaler{
		Unmarshaler: unmarshaler,
		encoding:    encoding,
	}
}

type pdataMetricsUnmarshaler struct {
	pmetric.Unmarshaler
	encoding string
}

func (p pdataMetricsUnmarshaler) Unmarshal(buf []byte) (pmetric.Metrics, error) {
	return p.UnmarshalMetrics(buf)
}

func (p pdataMetricsUnmarshaler) Encoding() string {
	return p.encoding
}

func newPdataMetricsUnmarshaler(unmarshaler pmetric.Unmarshaler, encoding string) MetricsUnmarshaler {
	return pdataMetricsUnmarshaler{
		Unmarshaler: unmarshaler,
		encoding:    encoding,
	}
}
