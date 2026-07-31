package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
	"github.com/nbd-wtf/go-nostr"
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
		want []relayOption
	}{
		{"single", "wss://relay.example.com", []relayOption{{URL: "wss://relay.example.com"}}},
		{"multiple", "wss://a.com,wss://b.com", []relayOption{{URL: "wss://a.com"}, {URL: "wss://b.com"}}},
		{"with spaces", " wss://a.com , wss://b.com ", []relayOption{{URL: "wss://a.com"}, {URL: "wss://b.com"}}},
		{"empty entries skipped", "wss://a.com,,wss://b.com", []relayOption{{URL: "wss://a.com"}, {URL: "wss://b.com"}}},
		{"empty string", "", nil},
		{"only commas and spaces", " , , ", nil},
		{
			"auth and group",
			"wss://vim-jp.communities.buzz.xyz?auth=true&group=xxx",
			[]relayOption{{URL: "wss://vim-jp.communities.buzz.xyz", Auth: true, Group: "xxx"}},
		},
		{
			"group only",
			"wss://a.com?group=yyy",
			[]relayOption{{URL: "wss://a.com", Group: "yyy"}},
		},
		{
			"auth false",
			"wss://a.com?auth=false",
			[]relayOption{{URL: "wss://a.com"}},
		},
		{
			"mixed plain and group",
			"wss://a.com,wss://b.com?auth=1&group=zzz",
			[]relayOption{{URL: "wss://a.com"}, {URL: "wss://b.com", Auth: true, Group: "zzz"}},
		},
		{
			"unrelated query params kept",
			"wss://a.com?auth=true&foo=bar",
			[]relayOption{{URL: "wss://a.com?foo=bar", Auth: true}},
		},
		{
			"group name url-encoded",
			"wss://a.com?auth=true&group=%23foo",
			[]relayOption{{URL: "wss://a.com", Auth: true, Group: "#foo"}},
		},
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
			name: "a text same as href not duplicated",
			in:   `<a href="https://example.com/">https://example.com/</a>`,
			want: "https://example.com/",
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

func TestGroupMetadataID(t *testing.T) {
	mk := func(createdAt int64, id, name string) *nostr.Event {
		return &nostr.Event{
			Kind:      nostr.KindSimpleGroupMetadata,
			CreatedAt: nostr.Timestamp(createdAt),
			Tags:      nostr.Tags{{"d", id}, {"name", name}},
		}
	}
	evs := []*nostr.Event{
		mk(100, "id-old", "foo"),
		mk(200, "id-new", "foo"),
		mk(300, "id-bar", "bar"),
		{Kind: nostr.KindSimpleGroupMetadata, CreatedAt: 400, Tags: nostr.Tags{{"name", "noid"}}},
	}

	if got, err := groupMetadataID(evs, "foo"); err != nil || got != "id-new" {
		t.Errorf("groupMetadataID(foo) = %q, %v, want id-new", got, err)
	}
	if got, err := groupMetadataID(evs, "bar"); err != nil || got != "id-bar" {
		t.Errorf("groupMetadataID(bar) = %q, %v, want id-bar", got, err)
	}
	if _, err := groupMetadataID(evs, "missing"); err == nil {
		t.Error("groupMetadataID(missing) should fail")
	}
	if _, err := groupMetadataID(evs, "noid"); err == nil {
		t.Error("groupMetadataID(noid) should fail for metadata without d tag")
	}
}
