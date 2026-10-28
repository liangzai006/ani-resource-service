package vmv1

import (
	"bytes"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestDNSCanonicalCompatibilityAndNormalizedCustomIntent(t *testing.T) {
	s := fixedSpec(t)
	legacy, e := CanonicalCreateSpec(s)
	if e != nil || bytes.Contains(legacy, []byte(`"dns"`)) {
		t.Fatal("legacy spec acquired DNS", e)
	}
	legacyIntent, _ := CanonicalUserIntent(s.Intent)
	for _, selector := range []*DNSSelection{{}, {Mode: DNSPlatformDefault}} {
		s.Intent.Dns = selector
		got, e := CanonicalUserIntent(s.Intent)
		if e != nil || !bytes.Equal(got, legacyIntent) {
			t.Fatal("default selection changed legacy request digest", e)
		}
		got, e = CanonicalCreateSpec(s)
		if e != nil || !bytes.Equal(got, legacy) {
			t.Fatal("default selection changed legacy frozen digest", e)
		}
	}
	s.Intent.Dns = &DNSSelection{Mode: DNSCustom, Nameservers: []string{"2001:0db8:0000:0::1", "::ffff:223.5.5.5", "2001:db8::1"}}
	s.Dns = &FrozenDNS{Mode: DNSCustom, Nameservers: []string{"2001:db8::1", "223.5.5.5"}}
	canonical, e := CanonicalCreateSpec(s)
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := DecodeCreateSpec(canonical)
	if e != nil || !slices.Equal(decoded.Intent.Dns.Nameservers, s.Dns.Nameservers) || !proto.Equal(decoded.Dns, s.Dns) {
		t.Fatal("normalized DNS did not survive frozen round trip", e)
	}
	changed := proto.Clone(decoded).(*CreateSpec)
	changed.Intent.Dns.Nameservers = []string{"223.5.5.5", "2001:db8::1"}
	changed.Dns.Nameservers = append([]string(nil), changed.Intent.Dns.Nameservers...)
	changedBytes, e := CanonicalCreateSpec(changed)
	if e != nil || bytes.Equal(canonical, changedBytes) {
		t.Fatal("resolver preference order was not bound to intent", e)
	}
	old, e := DecodeCreateSpec(legacy)
	if e != nil || old.Dns != nil {
		t.Fatal("legacy spec cannot be read", e)
	}
	again, _ := CanonicalCreateSpec(old)
	if !bytes.Equal(legacy, again) {
		t.Fatal("legacy accepted digest changed")
	}
}

func TestDNSRejectsInvalidSelectionsAndUnfrozenCustomAddresses(t *testing.T) {
	for _, selector := range []*DNSSelection{
		{Mode: "UNKNOWN"},
		{Nameservers: []string{"223.5.5.5"}},
		{Mode: DNSPlatformDefault, Nameservers: []string{"223.5.5.5"}},
		{Mode: DNSCustom},
		{Mode: DNSCustom, Nameservers: []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "223.5.5.5"}},
		{Mode: DNSCustom, Nameservers: []string{"dns.example.com"}},
		{Mode: DNSCustom, Nameservers: []string{"223.5.5.5:53"}},
		{Mode: DNSCustom, Nameservers: []string{"10.0.0.0/8"}},
		{Mode: DNSCustom, Nameservers: []string{"fe80::1%eth0"}},
		{Mode: DNSCustom, Nameservers: []string{" 223.5.5.5"}},
	} {
		s := fixedSpec(t)
		s.Intent.Dns = selector
		if _, e := CanonicalUserIntent(s.Intent); e == nil {
			t.Fatalf("invalid DNS accepted: %+v", selector)
		}
	}
	s := fixedSpec(t)
	s.Intent.Dns = &DNSSelection{Mode: DNSCustom, Nameservers: []string{"223.5.5.5"}}
	if ValidateCreateSpec(s) == nil {
		t.Fatal("custom request without frozen DNS accepted")
	}
	s.Dns = &FrozenDNS{Mode: DNSCustom, Nameservers: []string{"1.1.1.1"}}
	if ValidateCreateSpec(s) == nil {
		t.Fatal("frozen DNS differed from the user choice")
	}
	s.Intent.Dns = nil
	s.Dns = &FrozenDNS{Mode: DNSPlatformDefault}
	if ValidateCreateSpec(s) == nil {
		t.Fatal("new frozen platform DNS can be empty")
	}
}
