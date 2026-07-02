package piiredact

import (
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{"", ModeOff},
		{"off", ModeOff},
		{"observe", ModeObserve},
		{"REDACT", ModeRedact}, // case-insensitive
		{" block ", ModeBlock}, // trimmed
	}
	for _, c := range cases {
		got, err := ParseMode(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q, nil", c.in, got, err, c.want)
		}
	}
	if _, err := ParseMode("nonsense"); err == nil {
		t.Error("expected error for invalid mode")
	}
}

func TestOffModeIsNoOp(t *testing.T) {
	r := New(ModeOff)
	in := "email me at a@b.com or 555-12-1234"
	res := r.Redact(in)
	if res.Text != in || res.Found() || res.Blocked {
		t.Errorf("off mode should be a pure no-op, got %+v", res)
	}
}

func TestRedactEmail(t *testing.T) {
	res := New(ModeRedact).Redact("ping alice@example.com now")
	if res.Text != "ping [PII:email] now" {
		t.Errorf("email redaction: got %q", res.Text)
	}
	if !res.Found() || res.Findings[0].Kind != KindEmail || res.Findings[0].Count != 1 {
		t.Errorf("email finding wrong: %+v", res.Findings)
	}
}

func TestRedactSSN(t *testing.T) {
	res := New(ModeRedact).Redact("SSN 123-45-6789 on file")
	if res.Text != "SSN [PII:ssn] on file" {
		t.Errorf("ssn redaction: got %q", res.Text)
	}
}

func TestCreditCardLuhn(t *testing.T) {
	// 4242 4242 4242 4242 is a valid Luhn test card.
	valid := New(ModeRedact).Redact("card 4242 4242 4242 4242 ok")
	if !strings.Contains(valid.Text, "[PII:credit_card]") {
		t.Errorf("valid card should redact: %q", valid.Text)
	}
	// A 16-digit run that FAILS Luhn must not be flagged as a card.
	invalid := New(ModeRedact).Redact("order 1234 5678 9012 3456 shipped")
	if strings.Contains(invalid.Text, "[PII:credit_card]") {
		t.Errorf("non-Luhn digits should NOT be a card: %q", invalid.Text)
	}
}

func TestIPv4OctetValidation(t *testing.T) {
	ok := New(ModeObserve).Redact("host 192.168.1.42 down")
	if !ok.Found() || ok.Findings[0].Kind != KindIP {
		t.Errorf("valid IPv4 should be detected: %+v", ok.Findings)
	}
	// 999.1.2.3 has an out-of-range octet → not an IP.
	bad := New(ModeObserve).Redact("version 999.1.2.3 build")
	if bad.Found() {
		t.Errorf("out-of-range octet should not be an IP: %+v", bad.Findings)
	}
}

func TestPhoneConservative(t *testing.T) {
	// A separated NANP number is detected.
	got := New(ModeRedact).Redact("call 415-555-0132 today")
	if !strings.Contains(got.Text, "[PII:phone]") {
		t.Errorf("separated phone should redact: %q", got.Text)
	}
	// A bare 10-digit id (no separators) is deliberately NOT swept up.
	bare := New(ModeObserve).Redact("id 4155550132 ref")
	for _, f := range bare.Findings {
		if f.Kind == KindPhone {
			t.Errorf("bare digit run should not be a phone: %+v", bare.Findings)
		}
	}
}

func TestObserveModePassesTextThrough(t *testing.T) {
	in := "reach me: bob@corp.com"
	res := New(ModeObserve).Redact(in)
	if res.Text != in {
		t.Errorf("observe must not modify text, got %q", res.Text)
	}
	if !res.Found() || res.Blocked {
		t.Errorf("observe should report findings without blocking: %+v", res)
	}
}

func TestBlockModeWithholds(t *testing.T) {
	res := New(ModeBlock).Redact("secret contact carol@corp.com and 123-45-6789")
	if !res.Blocked {
		t.Error("block mode should set Blocked")
	}
	if strings.Contains(res.Text, "carol@corp.com") || strings.Contains(res.Text, "123-45-6789") {
		t.Errorf("block mode must not leak the raw values: %q", res.Text)
	}
	if !strings.HasPrefix(res.Text, "[BLOCKED:") {
		t.Errorf("block mode should return a block notice, got %q", res.Text)
	}
}

func TestNoPIILeavesTextUntouched(t *testing.T) {
	in := "the quarterly report is ready for review"
	for _, m := range []Mode{ModeObserve, ModeRedact, ModeBlock} {
		res := New(m).Redact(in)
		if res.Text != in || res.Found() || res.Blocked {
			t.Errorf("mode %q: clean text should be untouched, got %+v", m, res)
		}
	}
}

func TestMultipleFindingsCounted(t *testing.T) {
	res := New(ModeObserve).Redact("a@x.com, b@y.com, ip 10.0.0.1")
	counts := map[Kind]int{}
	for _, f := range res.Findings {
		counts[f.Kind] = f.Count
	}
	if counts[KindEmail] != 2 || counts[KindIP] != 1 {
		t.Errorf("expected email×2, ip×1, got %+v", res.Findings)
	}
	if res.Summary() == "" {
		t.Error("summary should be non-empty when findings exist")
	}
}

func TestNilRedactorNoOp(t *testing.T) {
	var r *PatternRedactor
	res := r.Redact("a@b.com")
	if res.Text != "a@b.com" || res.Found() {
		t.Errorf("nil redactor should be a no-op, got %+v", res)
	}
}
