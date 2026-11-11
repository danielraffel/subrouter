package selectacct

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	// DefaultMinNewSessionHeadroom preserves the historical admission reserve.
	DefaultMinNewSessionHeadroom = 0.40
	minNewSessionHeadroomFloor   = 0.05
	minNewSessionHeadroomCeiling = 0.90
	minStickyRetentionEpsilon    = 0.001
)

var minNewSessionHeadroomBits atomic.Uint64

func init() {
	setMinNewSessionHeadroom(DefaultMinNewSessionHeadroom)
}

// MinNewSessionHeadroom returns the process-wide startup-configured reserve.
func MinNewSessionHeadroom() float64 {
	return math.Float64frombits(minNewSessionHeadroomBits.Load())
}

// MinStickyRetentionHeadroom returns the idle-session threshold. It retains
// the historical 5% value, while staying strictly below a lowered admission
// threshold.
func MinStickyRetentionHeadroom() float64 {
	return math.Min(0.05, math.Max(0, MinNewSessionHeadroom()-minStickyRetentionEpsilon))
}

func setMinNewSessionHeadroom(value float64) {
	minNewSessionHeadroomBits.Store(math.Float64bits(value))
}

// ResolveMinNewSessionHeadroom applies the environment override first, then
// the optional existing config-file value, and finally the historical default.
// The returned warning is non-empty for invalid or clamped input.
func ResolveMinNewSessionHeadroom(envValue string, configValue *float64) (value float64, warning string, source string) {
	raw := strings.TrimSpace(envValue)
	source = "default"
	if raw == "" && configValue != nil {
		raw = strconv.FormatFloat(*configValue, 'g', -1, 64)
		source = "config"
	}
	if raw == "" {
		return DefaultMinNewSessionHeadroom, "", source
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 || parsed > 1 {
		return DefaultMinNewSessionHeadroom, fmt.Sprintf("invalid new-session headroom %q; using %.2f", raw, DefaultMinNewSessionHeadroom), source
	}
	value = parsed
	if value < minNewSessionHeadroomFloor {
		warning = fmt.Sprintf("new-session headroom %.4f is below the minimum; clamped to %.2f", value, minNewSessionHeadroomFloor)
		value = minNewSessionHeadroomFloor
	} else if value > minNewSessionHeadroomCeiling {
		warning = fmt.Sprintf("new-session headroom %.4f is above the maximum; clamped to %.2f", value, minNewSessionHeadroomCeiling)
		value = minNewSessionHeadroomCeiling
	}
	return value, warning, source
}

// ConfigureMinNewSessionHeadroom resolves and publishes the startup value.
func ConfigureMinNewSessionHeadroom(envValue string, configValue *float64) (value float64, warning string, source string) {
	value, warning, source = ResolveMinNewSessionHeadroom(envValue, configValue)
	setMinNewSessionHeadroom(value)
	return value, warning, source
}
