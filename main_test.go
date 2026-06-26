package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
	"github.com/nbd-wtf/go-nostr/nip19"
)

func TestExtractHashtags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"none", "no tags here", []string{}},
		{"single", "hello #world", []string{"world"}},
		{"multiple", "#a and #b and #c", []string{"a", "b", "c"}},
		{"japanese", "こんにちは #日本語 タグ", []string{"日本語"}},
		{"stops at punctuation", "see #tag, then #next.", []string{"tag", "next"}},
		{"stops at space", "#one #two", []string{"one", "two"}},
		{"ignores url fragments", "see https://example.com/post#section #tag", []string{"tag"}},
		{"ignores mid-word hash", "word#part #tag", []string{"tag"}},
		{"hash only", "#", []string{}},
		{"empty", "", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractHashtags(tt.in)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractHashtags(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseRelays(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"single", "wss://relay.example.com", []string{"wss://relay.example.com"}},
		{"multiple", "wss://a.com,wss://b.com", []string{"wss://a.com", "wss://b.com"}},
		{"with spaces", " wss://a.com , wss://b.com ", []string{"wss://a.com", "wss://b.com"}},
		{"empty entries skipped", "wss://a.com,,wss://b.com", []string{"wss://a.com", "wss://b.com"}},
		{"empty string", "", nil},
		{"only commas and spaces", " , , ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRelays(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseRelays(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestItemGUID(t *testing.T) {
	tests := []struct {
		name string
		item *gofeed.Item
		want string
	}{
		{
			name: "uses guid when present",
			item: &gofeed.Item{GUID: "guid-1", Link: "https://example.com/post"},
			want: "guid-1",
		},
		{
			name: "falls back to link when guid is empty",
			item: &gofeed.Item{Link: "https://example.com/post"},
			want: "https://example.com/post",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := itemGUID(tt.item)
			if got != tt.want {
				t.Errorf("itemGUID(%+v) = %q, want %q", tt.item, got, tt.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"squeeze newlines", "a\n\n\nb", "a\nb"},
		{"strip format chars", "a​b‌c", "abc"},
		{"single newline kept", "a\nb", "a\nb"},
		{"mixed", "a​\n\n\nb", "a\nb"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalize(tt.in)
			if got != tt.want {
				t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHtmlToText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "strip tags",
			in:   "<b>hello</b> <i>world</i>",
			want: "hello world",
		},
		{
			name: "img src preserved",
			in:   `before<img src="https://example.com/a.png">after`,
			want: "before\nhttps://example.com/a.png\nafter",
		},
		{
			name: "a href appended after text",
			in:   `<a href="https://example.com">click</a>`,
			want: "click https://example.com ",
		},
		{
			name: "br becomes newline",
			in:   "a<br>b",
			want: "a\nb",
		},
		{
			name: "block elements add newline",
			in:   "<p>a</p><div>b</div><li>c</li>",
			want: "\na\nb\nc",
		},
		{
			name: "nested tags",
			in:   `<p>see <a href="https://example.com">here</a> for more</p>`,
			want: "\nsee here https://example.com  for more",
		},
		{
			name: "img with other attrs",
			in:   `<img alt="x" src="https://example.com/a.png" width="10">`,
			want: "\nhttps://example.com/a.png\n",
		},
		{
			name: "a without href",
			in:   `<a>text</a>`,
			want: "text",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := htmlToText(tt.in)
			if got != tt.want {
				t.Errorf("htmlToText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPostNostrRejectsNonNsecKeys(t *testing.T) {
	nprofile, err := nip19.EncodeProfile("3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d", nil)
	if err != nil {
		t.Fatal(err)
	}

	err = postNostr(nprofile, nil, "https://example.com/post", "content")
	if err == nil {
		t.Fatal("postNostr returned nil error for nprofile key")
	}
	if !strings.Contains(err.Error(), "expected nsec private key") {
		t.Fatalf("postNostr error = %q, want expected nsec private key", err)
	}
}
