package tests

import (
	"os"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/common/testutil"
	"github.com/open-telemetry/opentelemetry-collector-contrib/testbed/testbed"
)

func TestRotelLog10kDPS(t *testing.T) {
	type test struct {
		name         string
		sender       testbed.DataSender
		receiver     testbed.DataReceiver
		resourceSpec testbed.ResourceSpec
		extensions   map[string]string
	}

	tests := []test{
		{
			name:     "OTLP",
			sender:   testbed.NewOTLPLogsDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 120,
			},
		},
		{
			name:     "OTLP-HTTP",
			sender:   testbed.NewOTLPHTTPLogsDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 120,
			},
		},
		{
			name:     "Rotel-OTLP",
			sender:   testbed.NewOTLPLogsDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 120,
			},
		},
		{
			name:     "Rotel-OTLP-HTTP",
			sender:   testbed.NewOTLPHTTPLogsDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 120,
			},
		},
		//{
		//	name:     "Fluentbit-OTLP",
		//	sender:   testbed.NewOTLPLogsDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
		//	receiver: testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
		//	resourceSpec: testbed.ResourceSpec{
		//		ExpectedMaxCPU: 30,
		//		ExpectedMaxRAM: 120,
		//	},
		//},
		{
			name:     "Fluentbit-OTLP-HTTP",
			sender:   testbed.NewOTLPHTTPLogsDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 120,
			},
		},
	}

	if e := os.Getenv("SKIP_FLUENTBIT_LOG10KDPS"); e == "" {
		tests = append(tests, test{
			name:     "Fluentbit-OTLP",
			sender:   testbed.NewOTLPLogsDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 120,
			},
		})
	}

	processors := []ProcessorNameAndConfigBody{
		{
			Name: "batch",
			Body: `
  batch:
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			Scenario10kItemsPerSecond(
				t,
				test.sender,
				test.receiver,
				test.resourceSpec,
				performanceResultsSummary,
				processors,
				test.extensions,
				nil,
			)
		})
	}
}

func TestRotelMetric10kDPS(t *testing.T) {
	tests := []struct {
		name         string
		sender       testbed.DataSender
		receiver     testbed.DataReceiver
		resourceSpec testbed.ResourceSpec
		skipMessage  string
	}{
		{
			name:     "OTLP",
			sender:   testbed.NewOTLPMetricDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 60,
				ExpectedMaxRAM: 105,
			},
		},
		{
			name:     "OTLP-HTTP",
			sender:   testbed.NewOTLPHTTPMetricDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 60,
				ExpectedMaxRAM: 100,
			},
		},
		{
			name:     "RotelOTLP",
			sender:   testbed.NewOTLPMetricDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 60,
				ExpectedMaxRAM: 105,
			},
		},
		{
			name:     "RotelOTLP-HTTP",
			sender:   testbed.NewOTLPHTTPMetricDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 60,
				ExpectedMaxRAM: 100,
			},
		},
		{
			name:     "FluentbitOTLP",
			sender:   testbed.NewOTLPMetricDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 60,
				ExpectedMaxRAM: 105,
			},
		},
		{
			name:     "FluentbitOTLP-HTTP",
			sender:   testbed.NewOTLPHTTPMetricDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			receiver: testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			resourceSpec: testbed.ResourceSpec{
				ExpectedMaxCPU: 60,
				ExpectedMaxRAM: 100,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.skipMessage != "" {
				t.Skip(test.skipMessage)
			}
			Scenario10kItemsPerSecond(
				t,
				test.sender,
				test.receiver,
				test.resourceSpec,
				performanceResultsSummary,
				nil,
				nil,
				nil,
			)
		})
	}
}

func TestRotelTrace10kSPS(t *testing.T) {
	tests := []struct {
		name         string
		sender       testbed.DataSender
		receiver     testbed.DataReceiver
		resourceSpec testbed.ResourceSpec
	}{
		{
			"OTLP-gRPC",
			testbed.NewOTLPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 20,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"OTLP-gRPC-gzip",
			testbed.NewOTLPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)).WithCompression("gzip"),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"OTLP-HTTP",
			testbed.NewOTLPHTTPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t), ""),
			testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 20,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"OTLP-HTTP-gzip",
			testbed.NewOTLPHTTPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t), "gzip"),
			testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)).WithCompression("gzip"),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 25,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"Rotel-OTLP-gRPC",
			testbed.NewOTLPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 20,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"Rotel-OTLP-gRPC-gzip",
			testbed.NewOTLPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)).WithCompression("gzip"),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"Rotel-OTLP-HTTP",
			testbed.NewOTLPHTTPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t), ""),
			testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 20,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"Rotel-OTLP-HTTP-gzip",
			testbed.NewOTLPHTTPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t), "gzip"),
			testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)).WithCompression("gzip"),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 25,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"Fluentbit-OTLP-gRPC",
			testbed.NewOTLPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 20,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"Fluentbit-OTLP-gRPC-gzip",
			testbed.NewOTLPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t)),
			testbed.NewOTLPDataReceiver(testutil.GetAvailablePort(t)).WithCompression("gzip"),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 30,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"Fluentbit-OTLP-HTTP",
			testbed.NewOTLPHTTPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t), ""),
			testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 20,
				ExpectedMaxRAM: 100,
			},
		},
		{
			"Fluentbit-OTLP-HTTP-gzip",
			testbed.NewOTLPHTTPTraceDataSender(testbed.DefaultHost, testutil.GetAvailablePort(t), "gzip"),
			testbed.NewOTLPHTTPDataReceiver(testutil.GetAvailablePort(t)).WithCompression("gzip"),
			testbed.ResourceSpec{
				ExpectedMaxCPU: 25,
				ExpectedMaxRAM: 100,
			},
		},
	}

	processors := []ProcessorNameAndConfigBody{
		{
			Name: "batch",
			Body: `
  batch:
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			Scenario10kItemsPerSecond(
				t,
				test.sender,
				test.receiver,
				test.resourceSpec,
				performanceResultsSummary,
				processors,
				nil,
				nil,
			)
		})
	}
}

func TestRotelTrace1kSPSWithAttrs(t *testing.T) {
	for _, prefix := range []string{"", "Rotel-", "Fluentbit-"} {
		Scenario1kSPSWithAttrs(t, prefix, []string{}, []TestCase{
			// No attributes.
			{
				attrCount:      0,
				attrSizeByte:   0,
				expectedMaxCPU: 30,
				expectedMaxRAM: 150,
				resultsSummary: performanceResultsSummary,
			},

			// We generate 10 attributes each with average key length of 100 bytes and
			// average value length of 50 bytes so total size of attributes values is
			// 15000 bytes.
			{
				attrCount:      100,
				attrSizeByte:   50,
				expectedMaxCPU: 120,
				expectedMaxRAM: 150,
				resultsSummary: performanceResultsSummary,
			},

			// Approx 10 KiB attributes.
			{
				attrCount:      10,
				attrSizeByte:   1000,
				expectedMaxCPU: 100,
				expectedMaxRAM: 150,
				resultsSummary: performanceResultsSummary,
			},

			// Approx 100 KiB attributes.
			{
				attrCount:      20,
				attrSizeByte:   5000,
				expectedMaxCPU: 250,
				expectedMaxRAM: 150,
				resultsSummary: performanceResultsSummary,
			},
		}, nil, nil)
	}
}
