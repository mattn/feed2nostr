package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"net/url"
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

const version = "0.0.21"

var revision = "HEAD"

type Feed2Nostr struct {
	bun.BaseModel `bun:"table:feed2nostr,alias:f"`

	Feed      string    `bun:"feed,pk,notnull" json:"feed"`
	GUID      string    `bun:"guid,pk,notnull" json:"guid"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
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
	var sk string
	if prefix, s, err := nip19.Decode(nsec); err != nil {
		return err
	} else if prefix != "nsec" {
		return fmt.Errorf("expected nsec private key, got %s", prefix)
	} else {
		sk = s.(string)
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
			// The relay sends its NIP-42 challenge unsolicited right after the
			// connection opens; replying before it lands sends an empty
			// challenge, which some relays treat as a hard failure.
			time.Sleep(time.Second)
			actx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = relay.Auth(actx, func(aev *nostr.Event) error {
				return aev.Sign(sk)
			})
			cancel()
			if err != nil {
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
	flag.StringVar(&dsn, "dsn", os.Getenv("FEED2NOSTR_DSN"), "Database source")
	flag.StringVar(&feedURL, "feed", "", "Feed URL")
	flag.StringVar(&format, "format", "{{.Title | normalize}}\n{{.Link}}", "Post Format")
	flag.StringVar(&pattern, "pattern", "", "Match pattern")
	flag.StringVar(&nsec, "nsec", os.Getenv("FEED2NOSTR_NSEC"), "Nostr nsec")
	flag.StringVar(&relays, "relays", os.Getenv("FEED2NOSTR_RELAYS"), "Nostr relays (per-relay options as query params: ?auth=true&group=xxx)")
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

	rs = parseRelays(relays)
	if len(rs) == 0 {
		log.Fatal("must specify relays")
	}

	feed, err := gofeed.NewParser().ParseURL(feedURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
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
