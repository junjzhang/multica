package service

import "testing"

func TestIssueBrowserURL(t *testing.T) {
	cases := []struct {
		name     string
		appURL   string
		slug     string
		key      string
		expected string
	}{
		{name: "full", appURL: "https://app.example.test/", slug: "acme", key: "WORK-281", expected: "https://app.example.test/acme/issues/WORK-281"},
		{name: "escapes segments", appURL: "https://app.example.test", slug: "team one", key: "WORK 281", expected: "https://app.example.test/team%20one/issues/WORK%20281"},
		{name: "no app url", appURL: "", slug: "acme", key: "WORK-281", expected: ""},
		{name: "no slug", appURL: "https://app.example.test", slug: "", key: "WORK-281", expected: ""},
		{name: "no identifier", appURL: "https://app.example.test", slug: "acme", key: "", expected: ""},
		{name: "not a url", appURL: "app.example.test", slug: "acme", key: "WORK-281", expected: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IssueBrowserURL(tc.appURL, tc.slug, tc.key); got != tc.expected {
				t.Fatalf("IssueBrowserURL(%q, %q, %q) = %q, want %q", tc.appURL, tc.slug, tc.key, got, tc.expected)
			}
		})
	}
}

func TestTruncateDeliveryTitle(t *testing.T) {
	if got := truncateDeliveryTitle("  spaced  title  "); got != "spaced  title" {
		t.Fatalf("truncateDeliveryTitle trimmed = %q, want inner whitespace preserved", got)
	}
	if got := truncateDeliveryTitle("tab\nnl\u0007"); got != "tabnl" {
		t.Fatalf("truncateDeliveryTitle control characters = %q, want them dropped", got)
	}
	long := make([]rune, deliveryTitleLimit+10)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateDeliveryTitle(string(long))
	if []rune(got)[deliveryTitleLimit] != '…' || len([]rune(got)) != deliveryTitleLimit+1 {
		t.Fatalf("truncateDeliveryTitle long title did not end with an ellipsis at the limit")
	}
}
