package platform

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Resinat/Resin/internal/model"
)

func isLowerAlpha2(s string) bool {
	if len(s) != 2 {
		return false
	}
	return s[0] >= 'a' && s[0] <= 'z' && s[1] >= 'a' && s[1] <= 'z'
}

// ValidateRegionFilters validates region filters against lowercase ISO alpha-2 format.
// Entries may optionally be prefixed with "!" to indicate negation (e.g. !hk).
func ValidateRegionFilters(regionFilters []string) error {
	for i, r := range regionFilters {
		code := r
		if len(r) > 0 && r[0] == '!' {
			code = r[1:]
		}
		if !isLowerAlpha2(code) {
			return fmt.Errorf("region_filters[%d]: must be a 2-letter lowercase ISO 3166-1 alpha-2 code (e.g. us, jp) or negation (e.g. !hk)", i)
		}
	}
	return nil
}

// CompiledRegexFilter is a compiled node-name regex filter with polarity.
// Negative filters use a leading "!" in the persisted/API string form; the bang
// is meta-syntax and is not part of the compiled pattern.
type CompiledRegexFilter struct {
	Negative bool
	Re       *regexp.Regexp
}

// String returns a stable representation for diagnostics (includes "!" for negatives).
func (f CompiledRegexFilter) String() string {
	if f.Re == nil {
		if f.Negative {
			return "!"
		}
		return ""
	}
	if f.Negative {
		return "!" + f.Re.String()
	}
	return f.Re.String()
}

// CompileRegexFilters compiles regex filters in order.
// Entries may optionally be prefixed with "!" to mark exclusion; the prefix is
// stripped before compilation. A bare "!" (empty pattern after strip) is invalid.
func CompileRegexFilters(regexFilters []string) ([]CompiledRegexFilter, error) {
	compiled := make([]CompiledRegexFilter, 0, len(regexFilters))
	for i, raw := range regexFilters {
		negative := false
		pattern := raw
		if strings.HasPrefix(pattern, "!") {
			negative = true
			pattern = pattern[1:]
		}
		if pattern == "" {
			if negative {
				return nil, fmt.Errorf("regex_filters[%d]: exclusion pattern must be non-empty after stripping leading '!'", i)
			}
			return nil, fmt.Errorf("regex_filters[%d]: pattern must be non-empty", i)
		}
		c, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("regex_filters[%d]: invalid regex: %v", i, err)
		}
		compiled = append(compiled, CompiledRegexFilter{Negative: negative, Re: c})
	}
	return compiled, nil
}

// SplitCompiledRegexFilters separates include and exclude patterns.
func SplitCompiledRegexFilters(filters []CompiledRegexFilter) (include, exclude []*regexp.Regexp) {
	if len(filters) == 0 {
		return nil, nil
	}
	include = make([]*regexp.Regexp, 0, len(filters))
	exclude = make([]*regexp.Regexp, 0, len(filters))
	for _, f := range filters {
		if f.Re == nil {
			continue
		}
		if f.Negative {
			exclude = append(exclude, f.Re)
		} else {
			include = append(include, f.Re)
		}
	}
	return include, exclude
}

// PositiveRegexFilters wraps precompiled regexes as include-only filters (tests/helpers).
func PositiveRegexFilters(res ...*regexp.Regexp) []CompiledRegexFilter {
	if len(res) == 0 {
		return nil
	}
	out := make([]CompiledRegexFilter, 0, len(res))
	for _, re := range res {
		if re == nil {
			continue
		}
		out = append(out, CompiledRegexFilter{Re: re})
	}
	return out
}

// NewConfiguredPlatform builds a runtime platform with non-filter settings applied.
func NewConfiguredPlatform(
	id, name string,
	regexFilters []CompiledRegexFilter,
	regionFilters []string,
	stickyTTLNs int64,
	missAction string,
	emptyAccountBehavior string,
	fixedAccountHeader string,
	allocationPolicy string,
	passiveCircuitBreakerDisabled bool,
) *Platform {
	normalizedFixedHeaders, fixedHeaders, err := NormalizeFixedAccountHeaders(fixedAccountHeader)
	if err != nil {
		normalizedFixedHeaders = strings.TrimSpace(fixedAccountHeader)
		fixedHeaders = nil
	}
	plat := NewPlatform(id, name, regexFilters, regionFilters)
	plat.StickyTTLNs = stickyTTLNs
	plat.ReverseProxyMissAction = missAction
	plat.ReverseProxyEmptyAccountBehavior = emptyAccountBehavior
	plat.ReverseProxyFixedAccountHeader = normalizedFixedHeaders
	plat.ReverseProxyFixedAccountHeaders = append([]string(nil), fixedHeaders...)
	plat.AllocationPolicy = ParseAllocationPolicy(allocationPolicy)
	plat.PassiveCircuitBreakerDisabled = passiveCircuitBreakerDisabled
	return plat
}

// CompileModelRegexFilters compiles regex filters from persisted model values.
func CompileModelRegexFilters(platformID string, regexFilters []string) ([]CompiledRegexFilter, error) {
	compiled, err := CompileRegexFilters(regexFilters)
	if err != nil {
		return nil, fmt.Errorf("decode platform %s regex_filters: %w", platformID, err)
	}
	return compiled, nil
}

// BuildFromModel builds a runtime platform from a persisted model.Platform.
func BuildFromModel(mp model.Platform) (*Platform, error) {
	regexFilters, err := CompileModelRegexFilters(mp.ID, mp.RegexFilters)
	if err != nil {
		return nil, err
	}
	if err := ValidateRegionFilters(mp.RegionFilters); err != nil {
		return nil, err
	}
	emptyAccountBehavior := mp.ReverseProxyEmptyAccountBehavior
	if !ReverseProxyEmptyAccountBehavior(emptyAccountBehavior).IsValid() {
		emptyAccountBehavior = string(ReverseProxyEmptyAccountBehaviorRandom)
	}
	missAction := NormalizeReverseProxyMissAction(mp.ReverseProxyMissAction)
	if missAction == "" {
		return nil, fmt.Errorf(
			"decode platform %s reverse_proxy_miss_action: invalid value %q",
			mp.ID,
			mp.ReverseProxyMissAction,
		)
	}
	fixedHeader, _, err := NormalizeFixedAccountHeaders(mp.ReverseProxyFixedAccountHeader)
	if err != nil {
		return nil, fmt.Errorf("decode platform %s reverse_proxy_fixed_account_header: %w", mp.ID, err)
	}
	if emptyAccountBehavior == string(ReverseProxyEmptyAccountBehaviorFixedHeader) && fixedHeader == "" {
		return nil, fmt.Errorf(
			"decode platform %s reverse_proxy_fixed_account_header: required when reverse_proxy_empty_account_behavior is %s",
			mp.ID,
			ReverseProxyEmptyAccountBehaviorFixedHeader,
		)
	}

	return NewConfiguredPlatform(
		mp.ID,
		mp.Name,
		regexFilters,
		append([]string(nil), mp.RegionFilters...),
		mp.StickyTTLNs,
		string(missAction),
		emptyAccountBehavior,
		fixedHeader,
		mp.AllocationPolicy,
		mp.PassiveCircuitBreakerDisabled,
	), nil
}
