package sip

import "strings"

// BroadcastRule describes a broadcast-enabled address and the contact URIs that
// should ring in parallel when that address receives an INVITE.
type BroadcastRule struct {
	Address string
	Targets []string
}

// BroadcastPolicy exposes broadcast ringing targets keyed by their address of
// record.
type BroadcastPolicy struct {
	targets map[string][]string
}

// NewBroadcastPolicy builds a BroadcastPolicy from the supplied rules.
func NewBroadcastPolicy(rules []BroadcastRule) *BroadcastPolicy {
	policy := &BroadcastPolicy{targets: make(map[string][]string)}
	for _, rule := range rules {
		addr := normaliseBroadcastAddress(rule.Address)
		if addr == "" {
			continue
		}
		cleaned := make([]string, 0, len(rule.Targets))
		for _, target := range rule.Targets {
			target = strings.TrimSpace(target)
			if target == "" {
				continue
			}
			cleaned = append(cleaned, target)
		}
		copyTargets := make([]string, len(cleaned))
		copy(copyTargets, cleaned)
		policy.targets[addr] = copyTargets
	}
	return policy
}

// Targets returns a copy of the broadcast targets configured for the given
// address. The lookup is case-insensitive and ignores surrounding whitespace.
func (p *BroadcastPolicy) Targets(address string) []string {
	if p == nil || len(p.targets) == 0 {
		return nil
	}
	addr := normaliseBroadcastAddress(address)
	if addr == "" {
		return nil
	}
	targets, ok := p.targets[addr]
	if !ok {
		return nil
	}
	out := make([]string, len(targets))
	copy(out, targets)
	return out
}

// Has reports whether the policy defines a broadcast rule for the provided address.
func (p *BroadcastPolicy) Has(address string) bool {
	if p == nil || len(p.targets) == 0 {
		return false
	}
	addr := normaliseBroadcastAddress(address)
	if addr == "" {
		return false
	}
	_, ok := p.targets[addr]
	return ok
}

func normaliseBroadcastAddress(address string) string {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return ""
	}
	return strings.ToLower(trimmed)
}
