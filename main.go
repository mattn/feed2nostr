package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	_ "github.com/lib/pq"
	"github.com/mmcdole/gofeed"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"golang.org/x/net/html"
)

const name = "feed2nostr"

const version = "0.0.23"

// kinds used by -podcast: a replaceable event describing the show and one
// event per episode carrying the audio url.
const (
	kindPodcastEpisode = 54
	kindPodcastShow    = 10154
)

var revision = "HEAD"

type Feed2Nostr struct {
	bun.BaseModel `bun:"table:feed2nostr,alias:f"`

	Feed      string    `bun:"feed,pk,notnull" json:"feed"`
	GUID      string    `bun:"guid,pk,notnull" json:"guid"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// Feed2NostrPodcast tracks which items have been published as podcast episodes.
// It is kept apart from Feed2Nostr so that turning -podcast on for a feed that
// has been posting kind 1 notes for a while still publishes the episodes that
// are in the feed, instead of considering them all done.
type Feed2NostrPodcast struct {
	bun.BaseModel `bun:"table:feed2nostr_podcast,alias:p"`

	Feed      string    `bun:"feed,pk,notnull" json:"feed"`
	GUID      string    `bun:"guid,pk,notnull" json:"guid"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// Feed2NostrPodcastShow remembers the last published show description so the
// replaceable kind 10154 event is only sent again when the feed changed it.
type Feed2NostrPodcastShow struct {
	bun.BaseModel `bun:"table:feed2nostr_podcast_show,alias:ps"`

	Feed      string    `bun:"feed,pk,notnull" json:"feed"`
	Hash      string    `bun:"hash,notnull" json:"hash"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

var hashtagRE = regexp.MustCompile(`(^|\s)#([^\s!@#$%^&*()=+.\/,\[{\]};:'"?><]+)`)

func extractHashtags(s string) []string {
	matches := hashtagRE.FindAllStringSubmatch(s, -1)
	tags := make([]string, 0, len(matches))
	for _, m := range matches {
		tags = append(tags, m[2])
	}
	return tags
}

// relayOption is a relay URL with per-relay options parsed from its query
// string, e.g. wss://example.communities.buzz.xyz?auth=true&group=xxx
// A group may also be referenced by name as "#foo" (URL-encoded as group=%23foo),
// resolved via the relay's kind 39000 metadata before posting.
type relayOption struct {
	URL   string
	Auth  bool   // authenticate with NIP-42 before publishing
	Group string // NIP-29 group id: post as kind 9 with an "h" tag
}

func parseRelays(s string) []relayOption {
	var rs []relayOption
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		u, err := url.Parse(r)
		if err != nil {
			rs = append(rs, relayOption{URL: r})
			continue
		}
		q := u.Query()
		opt := relayOption{}
		opt.Auth, _ = strconv.ParseBool(q.Get("auth"))
		opt.Group = q.Get("group")
		q.Del("auth")
		q.Del("group")
		u.RawQuery = q.Encode()
		opt.URL = u.String()
		rs = append(rs, opt)
	}
	return rs
}

func itemGUID(item *gofeed.Item) string {
	if item.GUID != "" {
		return item.GUID
	}
	return item.Link
}

func normalize(s string) string {
	// Remove invisible Unicode characters and squeeze multiple newlines
	s = regexp.MustCompile(`[\p{Cf}]`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`\n\n+`).ReplaceAllString(s, "\n")
	return s
}

func attrValue(z *html.Tokenizer, name string) string {
	for {
		k, v, more := z.TagAttr()
		if string(k) == name {
			return string(v)
		}
		if !more {
			return ""
		}
	}
}

func htmlToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	type anchor struct {
		href string
		pos  int
	}
	var anchors []anchor
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.TextToken:
			b.Write(z.Text())
		case html.StartTagToken, html.SelfClosingTagToken:
			tn, hasAttr := z.TagName()
			switch string(tn) {
			case "br", "p", "div", "li":
				b.WriteString("\n")
			case "img":
				if hasAttr {
					if src := attrValue(z, "src"); src != "" {
						b.WriteString("\n")
						b.WriteString(src)
						b.WriteString("\n")
					}
				}
			case "a":
				href := ""
				if hasAttr {
					href = attrValue(z, "href")
				}
				if tt == html.SelfClosingTagToken {
					if href != "" {
						b.WriteString(" ")
						b.WriteString(href)
						b.WriteString(" ")
					}
				} else {
					anchors = append(anchors, anchor{href: href, pos: b.Len()})
				}
			}
		case html.EndTagToken:
			tn, _ := z.TagName()
			if string(tn) == "a" && len(anchors) > 0 {
				a := anchors[len(anchors)-1]
				anchors = anchors[:len(anchors)-1]
				// Skip appending the href when the anchor text is already
				// the same URL, to avoid emitting the link twice.
				if a.href != "" && strings.TrimSpace(b.String()[a.pos:]) != a.href {
					b.WriteString(" ")
					b.WriteString(a.href)
					b.WriteString(" ")
				}
			}
		}
	}
	return b.String()
}

func decodeNsec(nsec string) (string, error) {
	prefix, s, err := nip19.Decode(nsec)
	if err != nil {
		return "", err
	}
	if prefix != "nsec" {
		return "", fmt.Errorf("expected nsec private key, got %s", prefix)
	}
	return s.(string), nil
}

// authRelay performs NIP-42 authentication. The relay sends its challenge
// unsolicited right after the connection opens; replying before it lands sends
// an empty challenge, which some relays treat as a hard failure.
func authRelay(ctx context.Context, relay *nostr.Relay, sk string) error {
	time.Sleep(time.Second)
	actx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return relay.Auth(actx, func(aev *nostr.Event) error {
		return aev.Sign(sk)
	})
}

// groupMetadataID returns the group id ("d" tag) of the newest kind 39000
// event whose name tag matches name.
func groupMetadataID(evs []*nostr.Event, name string) (string, error) {
	var best *nostr.Event
	var bestID string
	for _, e := range evs {
		var id, n string
		for _, tag := range e.Tags {
			if len(tag) < 2 {
				continue
			}
			switch tag[0] {
			case "d":
				id = tag[1]
			case "name":
				n = tag[1]
			}
		}
		if n == name && id != "" {
			if best == nil || e.CreatedAt > best.CreatedAt {
				best = e
				bestID = id
			}
		}
	}
	if best == nil {
		return "", fmt.Errorf("no group named %q found", name)
	}
	return bestID, nil
}

// resolveGroups replaces "#name" group references with their group ids looked
// up via the relay's kind 39000 metadata.
func resolveGroups(nsec string, rs []relayOption) error {
	sk, err := decodeNsec(nsec)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for i := range rs {
		if !strings.HasPrefix(rs[i].Group, "#") {
			continue
		}
		name := strings.TrimPrefix(rs[i].Group, "#")
		relay, err := nostr.RelayConnect(ctx, rs[i].URL)
		if err != nil {
			return fmt.Errorf("%v: %w", rs[i].URL, err)
		}
		if rs[i].Auth {
			if err := authRelay(ctx, relay, sk); err != nil {
				log.Printf("%v: auth: %v", rs[i].URL, err)
			}
		}
		evs, err := relay.QuerySync(ctx, nostr.Filter{
			Kinds: []int{nostr.KindSimpleGroupMetadata},
		})
		relay.Close()
		if err != nil {
			return fmt.Errorf("%v: %w", rs[i].URL, err)
		}
		id, err := groupMetadataID(evs, name)
		if err != nil {
			return fmt.Errorf("%v: %w", rs[i].URL, err)
		}
		rs[i].Group = id
	}
	return nil
}

// buildEvent constructs and signs the event for one relay configuration. A
// plain relay gets a kind 1 note; a relay with a group gets a kind 9 NIP-29
// group message carrying the group id in the "h" tag.
func buildEvent(sk string, pub string, link string, content string, group string) (*nostr.Event, error) {
	ev := nostr.Event{}
	ev.PubKey = pub
	ev.Content = content
	ev.CreatedAt = nostr.Now()
	ev.Tags = nostr.Tags{}
	if group != "" {
		ev.Kind = nostr.KindSimpleGroupChatMessage
		ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"h", group})
	} else {
		ev.Kind = nostr.KindTextNote
	}
	ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"proxy", link, "rss"})
	ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"client", name})

	for _, h := range extractHashtags(ev.Content) {
		ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"t", h})
	}

	if err := ev.Sign(sk); err != nil {
		return nil, err
	}
	return &ev, nil
}

func postNostr(nsec string, rs []relayOption, link string, content string) error {
	sk, err := decodeNsec(nsec)
	if err != nil {
		return err
	}
	pub, err := nostr.GetPublicKey(sk)
	if err != nil {
		return err
	}
	if _, err := nip19.EncodePublicKey(pub); err != nil {
		return err
	}

	events := map[string]*nostr.Event{}
	success := 0
	ctx := context.Background()
	for _, r := range rs {
		ev, ok := events[r.Group]
		if !ok {
			ev, err = buildEvent(sk, pub, link, content, r.Group)
			if err != nil {
				return err
			}
			events[r.Group] = ev
		}
		relay, err := nostr.RelayConnect(context.Background(), r.URL)
		if err != nil {
			log.Printf("%v: %v", r.URL, err)
			continue
		}
		if r.Auth {
			if err := authRelay(ctx, relay, sk); err != nil {
				log.Printf("%v: auth: %v", r.URL, err)
			}
		}
		err = relay.Publish(ctx, *ev)
		relay.Close()
		if err == nil {
			success++
		}
	}
	if success == 0 {
		return errors.New("failed to publish")
	}
	return nil
}

func main() {
	var skip bool
	var podcast bool
	var dsn string
	var feedURL string
	var format string
	var pattern string
	var re *regexp.Regexp
	var nsec string
	var relays string
	var rs []relayOption
	var ver bool

	flag.BoolVar(&skip, "skip", false, "Skip post")
	flag.BoolVar(&podcast, "podcast", false, "Also publish the feed as a podcast (kind 10154 and kind 54)")
	flag.StringVar(&dsn, "dsn", os.Getenv("FEED2NOSTR_DSN"), "Database source")
	flag.StringVar(&feedURL, "feed", "", "Feed URL")
	flag.StringVar(&format, "format", "{{.Title | normalize}}\n{{.Link}}", "Post Format")
	flag.StringVar(&pattern, "pattern", "", "Match pattern")
	flag.StringVar(&nsec, "nsec", os.Getenv("FEED2NOSTR_NSEC"), "Nostr nsec")
	flag.StringVar(&relays, "relays", os.Getenv("FEED2NOSTR_RELAYS"), "Nostr relays (per-relay options as query params: ?auth=true&group=xxx or group=%23name)")
	flag.BoolVar(&ver, "v", false, "show version")
	flag.Parse()

	if ver {
		fmt.Println(version)
		os.Exit(0)
	}

	var err error
	if pattern != "" {
		re, err = regexp.Compile(pattern)
		if err != nil {
			log.Fatal(err)
		}
	}

	funcMap := template.FuncMap{
		"normalize": normalize,
		"text":      htmlToText,
	}
	t := template.Must(template.New("").Funcs(funcMap).Parse(format))

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}

	bundb := bun.NewDB(db, pgdialect.New())
	defer bundb.Close()

	_, err = bundb.NewCreateTable().Model((*Feed2Nostr)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}

	if podcast {
		for _, model := range []any{(*Feed2NostrPodcast)(nil), (*Feed2NostrPodcastShow)(nil)} {
			if _, err := bundb.NewCreateTable().Model(model).IfNotExists().Exec(context.Background()); err != nil {
				log.Println(err)
				return
			}
		}
	}

	rs = parseRelays(relays)
	if len(rs) == 0 {
		log.Fatal("must specify relays")
	}
	if !skip {
		if err := resolveGroups(nsec, rs); err != nil {
			log.Fatal(err)
		}
	}

	feed, err := gofeed.NewParser().ParseURL(feedURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	if podcast {
		postPodcast(context.Background(), bundb, nsec, rs, feedURL, feed, skip)
	}

	for _, item := range feed.Items {
		if item == nil {
			break
		}

		fi := Feed2Nostr{
			Feed: feedURL,
			GUID: itemGUID(item),
		}
		_, err := bundb.NewInsert().Model(&fi).Exec(context.Background())
		if err != nil {
			if !strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
				log.Println(err)
			}
			continue
		}

		var buf bytes.Buffer
		err = t.Execute(&buf, &item)
		if err != nil {
			log.Println(err)
			continue
		}

		content := buf.String()

		if re != nil {
			if !re.MatchString(content) {
				continue
			}
		}

		if skip {
			log.Printf("%q", content)
			continue
		}

		err = postNostr(nsec, rs, item.Link, content)
		if err != nil {
			log.Println(err)
			if _, deleteErr := bundb.NewDelete().Model(&fi).WherePK().Exec(context.Background()); deleteErr != nil {
				log.Println(deleteErr)
			}
			continue
		}
	}
}

// podcastShowEvent builds the replaceable kind 10154 event that describes the
// show itself, from the channel level fields of the feed.
func podcastShowEvent(sk string, pub string, feed *gofeed.Feed) (*nostr.Event, error) {
	ev := nostr.Event{
		PubKey:    pub,
		Kind:      kindPodcastShow,
		CreatedAt: nostr.Now(),
		Content:   "",
		Tags:      nostr.Tags{nostr.Tag{"title", strings.TrimSpace(feed.Title)}},
	}

	if desc := normalize(htmlToText(feed.Description)); desc != "" {
		ev.Tags = append(ev.Tags, nostr.Tag{"description", desc})
	}
	if feed.Image != nil && feed.Image.URL != "" {
		ev.Tags = append(ev.Tags, nostr.Tag{"image", feed.Image.URL})
	} else if feed.ITunesExt != nil && feed.ITunesExt.Image != "" {
		ev.Tags = append(ev.Tags, nostr.Tag{"image", feed.ITunesExt.Image})
	}
	if feed.Link != "" {
		ev.Tags = append(ev.Tags, nostr.Tag{"website", feed.Link})
	}
	ev.Tags = append(ev.Tags, nostr.Tag{"client", name})

	if err := ev.Sign(sk); err != nil {
		return nil, err
	}
	return &ev, nil
}

// podcastEpisodeEvent builds a kind 54 event for one feed item. It returns nil
// when the item carries no audio, since there would be nothing to play.
func podcastEpisodeEvent(sk string, pub string, item *gofeed.Item) (*nostr.Event, error) {
	var enclosure *gofeed.Enclosure
	for _, e := range item.Enclosures {
		if e != nil && e.URL != "" {
			enclosure = e
			break
		}
	}
	if enclosure == nil {
		return nil, nil
	}

	audio := nostr.Tag{"audio", enclosure.URL}
	if enclosure.Type != "" {
		audio = append(audio, enclosure.Type)
	}

	ev := nostr.Event{
		PubKey:    pub,
		Kind:      kindPodcastEpisode,
		CreatedAt: nostr.Now(),
		Content:   normalize(htmlToText(item.Description)),
		Tags: nostr.Tags{
			nostr.Tag{"title", strings.TrimSpace(item.Title)},
			audio,
		},
	}

	if guid := itemGUID(item); guid != "" {
		// NIP-73, so the episode can be matched back to the feed item
		ev.Tags = append(ev.Tags, nostr.Tag{"i", "podcast:item:guid:" + guid})
	}
	if item.ITunesExt != nil {
		if seconds := durationSeconds(item.ITunesExt.Duration); seconds > 0 {
			ev.Tags = append(ev.Tags, nostr.Tag{"duration", strconv.Itoa(seconds)})
		}
	}
	if item.Link != "" {
		ev.Tags = append(ev.Tags, nostr.Tag{"r", item.Link})
	}
	ev.Tags = append(ev.Tags, nostr.Tag{"client", name})

	if item.PublishedParsed != nil {
		ev.CreatedAt = nostr.Timestamp(item.PublishedParsed.Unix())
	}

	if err := ev.Sign(sk); err != nil {
		return nil, err
	}
	return &ev, nil
}

// durationSeconds parses the itunes:duration field, which is either a number of
// seconds or a [hh:]mm:ss timestamp.
func durationSeconds(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0
	}
	total := 0
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// publishEvent sends an already built event to the relays. Relays configured
// with a NIP-29 group are skipped: a group is a chat room, not a feed.
func publishEvent(sk string, rs []relayOption, ev *nostr.Event) error {
	ctx := context.Background()
	success := 0
	for _, r := range rs {
		if r.Group != "" {
			continue
		}
		relay, err := nostr.RelayConnect(ctx, r.URL)
		if err != nil {
			log.Printf("%v: %v", r.URL, err)
			continue
		}
		if r.Auth {
			if err := authRelay(ctx, relay, sk); err != nil {
				log.Printf("%v: auth: %v", r.URL, err)
			}
		}
		err = relay.Publish(ctx, *ev)
		relay.Close()
		if err == nil {
			success++
		} else {
			log.Printf("%v: %v", r.URL, err)
		}
	}
	if success == 0 {
		return errors.New("failed to publish")
	}
	return nil
}

// postPodcast publishes the show event when it changed and one kind 54 event
// for each item that has not been published as an episode yet. Items are
// handled oldest first so the episodes land in the order they were released.
func postPodcast(ctx context.Context, bundb *bun.DB, nsec string, rs []relayOption, feedURL string, feed *gofeed.Feed, skip bool) {
	sk, err := decodeNsec(nsec)
	if err != nil {
		log.Println(err)
		return
	}
	pub, err := nostr.GetPublicKey(sk)
	if err != nil {
		log.Println(err)
		return
	}

	show, err := podcastShowEvent(sk, pub, feed)
	if err != nil {
		log.Println(err)
		return
	}

	hash := sha256.Sum256([]byte(fmt.Sprintf("%v%v", show.Tags, show.Content)))
	want := hex.EncodeToString(hash[:])

	var stored Feed2NostrPodcastShow
	err = bundb.NewSelect().Model(&stored).Where("feed = ?", feedURL).Scan(ctx)
	if err != nil || stored.Hash != want {
		if skip {
			log.Printf("%v", show)
		} else if err := publishEvent(sk, rs, show); err != nil {
			log.Println(err)
		} else {
			record := Feed2NostrPodcastShow{Feed: feedURL, Hash: want, UpdatedAt: time.Now()}
			if _, err := bundb.NewInsert().Model(&record).
				On("CONFLICT (feed) DO UPDATE").
				Set("hash = EXCLUDED.hash, updated_at = EXCLUDED.updated_at").
				Exec(ctx); err != nil {
				log.Println(err)
			}
		}
	}

	for i := len(feed.Items) - 1; i >= 0; i-- {
		item := feed.Items[i]
		if item == nil {
			continue
		}

		pi := Feed2NostrPodcast{Feed: feedURL, GUID: itemGUID(item)}
		if _, err := bundb.NewInsert().Model(&pi).Exec(ctx); err != nil {
			if !strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
				log.Println(err)
			}
			continue
		}

		ev, err := podcastEpisodeEvent(sk, pub, item)
		if err != nil {
			log.Println(err)
			continue
		}
		if ev == nil {
			continue // no audio, nothing to publish
		}

		if skip {
			log.Printf("%v", ev)
			continue
		}

		if err := publishEvent(sk, rs, ev); err != nil {
			log.Println(err)
			if _, deleteErr := bundb.NewDelete().Model(&pi).WherePK().Exec(ctx); deleteErr != nil {
				log.Println(deleteErr)
			}
		}
	}
}
