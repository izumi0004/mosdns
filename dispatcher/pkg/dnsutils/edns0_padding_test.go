package dnsutils

import (
	"github.com/miekg/dns"
	"strings"
	"testing"
)

func TestPadToMinimum(t *testing.T) {
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeA)

	qEDNS0 := q.Copy()
	UpgradeEDNS0(qEDNS0)

	qPadded := qEDNS0.Copy()
	opt := qPadded.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_PADDING{Padding: make([]byte, 16)})

	qLarge := new(dns.Msg)
	qLarge.SetQuestion(strings.Repeat("a.", 100), dns.TypeA)

	tests := []struct {
		name           string
		q              *dns.Msg
		minLen         int
		wantLen        int
		wantUpgraded   bool
		wantNewPadding bool
	}{
		{"", q.Copy(), 128, 128, true, true},
		{"", qLarge.Copy(), 128, qLarge.Len(), false, false},
		{"", qEDNS0.Copy(), 128, 128, false, true},
		{"", qPadded.Copy(), 128, 128, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUpgraded, gotNewPadding := PadToMinimum(tt.q, tt.minLen)
			if gotUpgraded != tt.wantUpgraded {
				t.Errorf("pad() gotUpgraded = %v, want %v", gotUpgraded, tt.wantUpgraded)
			}
			if gotNewPadding != tt.wantNewPadding {
				t.Errorf("pad() gotNewPadding = %v, want %v", gotNewPadding, tt.wantNewPadding)
			}
			if qLen := tt.q.Len(); qLen != tt.wantLen {
				t.Errorf("pad() query length = %v, want %v", qLen, tt.wantLen)
			}
		})
	}
}
