// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package prometheus // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/prometheus"

import (
	"strings"
	"unicode"

	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

var _ = featuregate.GlobalRegistry().MustRegister(
	"pkg.translator.prometheus.NormalizeName",
	featuregate.StageStable,
	featuregate.WithRegisterDescription("Controls whether metrics names are automatically normalized to follow Prometheus naming convention"),
	featuregate.WithRegisterReferenceURL("https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/8950"),
	featuregate.WithRegisterToVersion("v0.130.0"),
)

// BuildCompliantName builds a Prometheus-compliant metric name for the specified metric
//
// Metric name is prefixed with specified namespace and underscore (if any).
// Namespace is not cleaned up. Make sure specified namespace follows Prometheus
// naming convention.
//
// See rules at https://prometheus.io/docs/concepts/data_model/#metric-names-and-labels
// and https://prometheus.io/docs/practices/naming/#metric-and-label-naming
func BuildCompliantName(metric pmetric.Metric, namespace string, addMetricSuffixes bool) string {
	var metricName string

	// Full normalization following standard Prometheus naming conventions
	if addMetricSuffixes {
		return normalizeName(metric, namespace)
	}

	// Simple case (no full normalization, no units, etc.), we simply trim out forbidden chars
	metricName = RemovePromForbiddenRunes(metric.Name())

	// Namespace?
	if namespace != "" {
		return namespace + "_" + metricName
	}

	// Metric name starts with a digit? Prefix it with an underscore
	if metricName != "" && unicode.IsDigit(rune(metricName[0])) {
		metricName = "_" + metricName
	}

	return metricName
}

// Build a normalized name for the specified metric
func normalizeName(metric pmetric.Metric, namespace string) string {
	// Split metric name in "tokens" (remove all non-alphanumeric)
	nameTokens := strings.FieldsFunc(
		metric.Name(),
		func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) },
	)

	// Append unit if it exists
	promUnit, promUnitRate := buildCompliantMainUnit(metric.Unit()), buildCompliantPerUnit(metric.Unit())
	if promUnit != "" && !contains(nameTokens, promUnit) {
		nameTokens = append(nameTokens, promUnit)
	}
	if promUnitRate != "" && !contains(nameTokens, promUnitRate) {
		nameTokens = append(append(nameTokens, "per"), promUnitRate)
	}

	// Append _total for Counters
	if metric.Type() == pmetric.MetricTypeSum && metric.Sum().IsMonotonic() {
		nameTokens = append(removeItem(nameTokens, "total"), "total")
	}

	// Append _ratio for metrics with unit "1"
	// Some Otel receivers improperly use unit "1" for counters of objects
	// See https://github.com/open-telemetry/opentelemetry-collector-contrib/issues?q=is%3Aissue+some+metric+units+don%27t+follow+otel+semantic+conventions
	// Until these issues have been fixed, we're appending `_ratio` for gauges ONLY
	// Theoretically, counters could be ratios as well, but it's absurd (for mathematical reasons)
	if metric.Unit() == "1" && metric.Type() == pmetric.MetricTypeGauge {
		nameTokens = append(removeItem(nameTokens, "ratio"), "ratio")
	}

	// Namespace
	if namespace != "" {
		nameTokens = append([]string{namespace}, nameTokens...)
	}

	// Build the string from the tokens, separated with underscores
	normalizedName := strings.Join(nameTokens, "_")

	// Metric name cannot start with a digit, so prefix it with "_" in this case
	if normalizedName != "" && unicode.IsDigit(rune(normalizedName[0])) {
		normalizedName = "_" + normalizedName
	}

	return normalizedName
}

// TrimPromSuffixes trims type and unit prometheus suffixes from a metric name.
// Following the [OpenTelemetry specs] for converting Prometheus Metric points to OTLP.
//
// [OpenTelemetry specs]: https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/metrics/data-model.md#metric-metadata
func TrimPromSuffixes(promName string, metricType pmetric.MetricType, unit string) string {
	nameTokens := strings.Split(promName, "_")
	if len(nameTokens) == 1 {
		return promName
	}

	nameTokens = removeTypeSuffixes(nameTokens, metricType)
	nameTokens = removeUnitSuffixes(nameTokens, unit)

	return strings.Join(nameTokens, "_")
}

func removeTypeSuffixes(tokens []string, metricType pmetric.MetricType) []string {
	switch metricType {
	case pmetric.MetricTypeSum:
		// Only counters are expected to have a type suffix at this point.
		// for other types, suffixes are removed during scrape.
		return removeSuffix(tokens, "total")
	default:
		return tokens
	}
}

func removeUnitSuffixes(nameTokens []string, unit string) []string {
	l := len(nameTokens)
	unitTokens := strings.Split(unit, "_")
	lu := len(unitTokens)

	if lu == 0 || l <= lu {
		return nameTokens
	}

	suffixed := true
	for i := range unitTokens {
		if nameTokens[l-i-1] != unitTokens[lu-i-1] {
			suffixed = false
			break
		}
	}

	if suffixed {
		return nameTokens[:l-lu]
	}

	return nameTokens
}

func removeSuffix(tokens []string, suffix string) []string {
	l := len(tokens)
	if tokens[l-1] == suffix {
		return tokens[:l-1]
	}

	return tokens
}

func RemovePromForbiddenRunes(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != ':' }), "_")
}

// Returns whether the slice contains the specified value
func contains(slice []string, value string) bool {
	for _, sliceEntry := range slice {
		if sliceEntry == value {
			return true
		}
	}
	return false
}

// Remove the specified value from the slice
func removeItem(slice []string, value string) []string {
	newSlice := make([]string, 0, len(slice))
	for _, sliceEntry := range slice {
		if sliceEntry != value {
			newSlice = append(newSlice, sliceEntry)
		}
	}
	return newSlice
}
