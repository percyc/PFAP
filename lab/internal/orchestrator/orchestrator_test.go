package orchestrator

import "testing"

func TestShellQuote(t *testing.T) {
	got := shell("a'b c")
	want := "'a'\"'\"'b c'"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
func TestParseHash(t *testing.T) {
	want := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	if got := ParseHash("logs\n\"" + want + "\"\n"); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestParseProofTimes(t *testing.T) {
	p, v := ParseProofTimes("gen mint proof Use Time:30.182776s\nverify mint proof Use Time:0.008606s")
	if p != 30182 || v != 8 {
		t.Fatalf("proof=%d verify=%d", p, v)
	}
}

func TestParseProofTimesMicrosPreservesSubMillisecond(t *testing.T) {
	p, v := ParseProofTimesMicros("gen mint proof Use Time:0.000421s\nverify mint proof Use Time:0.000008s")
	if p != 421 || v != 8 {
		t.Fatalf("proof=%d verify=%d", p, v)
	}
}

func TestExtractTransactionTimingsUsesHashBoundary(t *testing.T) {
	logText := "gen mint proof Use Time:9s\nSubmitted transaction fullhash=0xold\nverify mint proof Use Time:7s\ngen mint proof Use Time:0.001234s\nverify mint proof Use Time:0.000421s\nSubmitted transaction fullhash=0xabc\n"
	p, v, ok := ExtractTransactionTimings(logText, "0xabc")
	if !ok || p != 1234 || v != 421 {
		t.Fatalf("proof=%d verify=%d ok=%v", p, v, ok)
	}
}

func TestExtractJSONString(t *testing.T) {
	got, err := ExtractJSONString("native log\n\"{\\\"proofA\\\":\\\"0x1\\\"}\"\n")
	if err != nil || got != `{"proofA":"0x1"}` {
		t.Fatalf("got=%q err=%v", got, err)
	}
}
