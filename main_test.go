package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/mmcdole/gofeed/extensions"
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

func TestDurationSeconds(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"seconds", "2320", 2320},
		{"mm:ss", "38:40", 2320},
		{"hh:mm:ss", "01:02:03", 3723},
		{"padded", "00:38:40", 2320},
		{"spaces", " 38:40 ", 2320},
		{"empty", "", 0},
		{"garbage", "about an hour", 0},
		{"too many parts", "1:2:3:4", 0},
		{"negative", "-5", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := durationSeconds(tt.in); got != tt.want {
				t.Errorf("durationSeconds(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func tagValue(ev *nostr.Event, name string) string {
	for _, tag := range ev.Tags {
		if len(tag) >= 2 && tag[0] == name {
			return tag[1]
		}
	}
	return ""
}

func TestPodcastEpisodeEvent(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pub, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}

	published := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	item := &gofeed.Item{
		Title:           "#22　えっとだからその",
		Description:     "<p>■トピック：部活</p><p>おたより募集中</p>",
		Link:            "https://example.com/episodes/22",
		GUID:            "f0470cca-0699-425d-89e9-a45eb67e0480",
		PublishedParsed: &published,
		Enclosures:      []*gofeed.Enclosure{{URL: "https://example.com/22.mp3", Type: "audio/mpeg"}},
		ITunesExt:       &ext.ITunesItemExtension{Duration: "00:38:40"},
	}

	ev, err := podcastEpisodeEvent(sk, pub, item)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil {
		t.Fatal("podcastEpisodeEvent() = nil, want an event")
	}

	if ev.Kind != kindPodcastEpisode {
		t.Errorf("kind = %d, want %d", ev.Kind, kindPodcastEpisode)
	}
	if got := tagValue(ev, "title"); got != item.Title {
		t.Errorf("title = %q, want %q", got, item.Title)
	}
	if got := tagValue(ev, "audio"); got != "https://example.com/22.mp3" {
		t.Errorf("audio = %q, want the enclosure url", got)
	}
	if got := tagValue(ev, "i"); got != "podcast:item:guid:"+item.GUID {
		t.Errorf("i = %q, want the nip73 guid", got)
	}
	if got := tagValue(ev, "duration"); got != "2320" {
		t.Errorf("duration = %q, want 2320", got)
	}
	if got := tagValue(ev, "r"); got != item.Link {
		t.Errorf("r = %q, want %q", got, item.Link)
	}
	if ev.CreatedAt != nostr.Timestamp(published.Unix()) {
		t.Errorf("created_at = %d, want the pubDate %d", ev.CreatedAt, published.Unix())
	}
	if strings.Contains(ev.Content, "<p>") {
		t.Errorf("content still has html: %q", ev.Content)
	}
	if !strings.Contains(ev.Content, "■トピック：部活") {
		t.Errorf("content lost the description: %q", ev.Content)
	}
	if ok, err := ev.CheckSignature(); err != nil || !ok {
		t.Errorf("signature does not verify: %v", err)
	}
}

func TestPodcastEpisodeEventWithoutAudio(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pub, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}

	ev, err := podcastEpisodeEvent(sk, pub, &gofeed.Item{Title: "no audio"})
	if err != nil {
		t.Fatal(err)
	}
	if ev != nil {
		t.Errorf("podcastEpisodeEvent() = %v, want nil for an item with no enclosure", ev)
	}
}

func TestPodcastShowEvent(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	pub, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}

	feed := &gofeed.Feed{
		Title:       "けだまメイズ",
		Description: "<p>ゆるい会話を残していきます</p>",
		Link:        "https://example.com/show",
		ITunesExt:   &ext.ITunesFeedExtension{Image: "https://example.com/cover.jpg"},
	}

	ev, err := podcastShowEvent(sk, pub, feed)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != kindPodcastShow {
		t.Errorf("kind = %d, want %d", ev.Kind, kindPodcastShow)
	}
	if got := tagValue(ev, "title"); got != feed.Title {
		t.Errorf("title = %q, want %q", got, feed.Title)
	}
	if got := tagValue(ev, "image"); got != "https://example.com/cover.jpg" {
		t.Errorf("image = %q, want the itunes image", got)
	}
	if got := tagValue(ev, "website"); got != feed.Link {
		t.Errorf("website = %q, want %q", got, feed.Link)
	}
	if got := tagValue(ev, "description"); strings.Contains(got, "<p>") {
		t.Errorf("description still has html: %q", got)
	}
	if ok, err := ev.CheckSignature(); err != nil || !ok {
		t.Errorf("signature does not verify: %v", err)
	}
}
