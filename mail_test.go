package cf_resend

import "testing"

func TestResolveFrom(t *testing.T) {
	got, err := resolveFrom("", "noreply@x.io")
	if err != nil || got != "noreply@x.io" {
		t.Fatalf("default = %q %v", got, err)
	}
	got, err = resolveFrom("  Brand <b@x.io>  ", "noreply@x.io")
	if err != nil || got != "Brand <b@x.io>" {
		t.Fatalf("override = %q %v", got, err)
	}
	if _, err := resolveFrom("", ""); err == nil {
		t.Fatal("both empty must fail")
	}
	if _, err := resolveFrom("not-an-email", "noreply@x.io"); err == nil {
		t.Fatal("invalid override must fail (not fall back)")
	}
	if _, err := resolveFrom("", "also-bad"); err == nil {
		t.Fatal("invalid default must fail")
	}
}

func TestValidateTo(t *testing.T) {
	if _, err := validateTo(nil); err == nil {
		t.Fatal("empty To must fail")
	}
	if _, err := validateTo([]string{"a@x.io", "nope"}); err == nil {
		t.Fatal("invalid To must fail")
	}
	got, err := validateTo([]string{"  a@x.io  "})
	if err != nil || len(got) != 1 || got[0] != "a@x.io" {
		t.Fatalf("trim To = %q %v", got, err)
	}
}

func TestMailToSDKTagsSorted(t *testing.T) {
	m := Mail{
		To:      []string{"a@x.io"},
		Subject: "s",
		Text:    "t",
		Tags:    map[string]string{"": "skip", "b": "2", "a": "1"},
	}
	req := m.toSDK("from@x.io")
	if req.From != "from@x.io" || req.Html != "" || req.Text != "t" {
		t.Fatalf("req = %+v", req)
	}
	if len(req.Tags) != 2 || req.Tags[0].Name != "a" || req.Tags[1].Name != "b" {
		t.Fatalf("tags = %+v", req.Tags)
	}
}
