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
	"strings"
	"text/template"
	"time"

	_ "github.com/lib/pq"
	"github.com/mmcdole/gofeed"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

const name = "feed2nostr"

const version = "0.0.1"

var revision = "HEAD"

type Feed2Nostr struct {
	bun.BaseModel `bun:"table:feed2nostr,alias:f"`

	Feed      string    `bun:"feed,pk,notnull" json:"feed"`
	GUID      string    `bun:"guid,pk,notnull" json:"guid"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

func postNostr(nsec string, content string) error {
	ev := nostr.Event{}
	var sk string
	if _, s, err := nip19.Decode(nsec); err != nil {
		return err
	} else {
		sk = s.(string)
	}
	if pub, err := nostr.GetPublicKey(sk); err == nil {
		if _, err := nip19.EncodePublicKey(pub); err != nil {
			return err
		}
		ev.PubKey = pub
	} else {
		return err
	}
	ev.Content = content
	ev.CreatedAt = nostr.Now()
	ev.Kind = nostr.KindTextNote
	ev.Tags = nostr.Tags{}
	ev.Sign(sk)

	success := 0
	ctx := context.Background()
	for _, r := range strings.Split(os.Getenv("FEED2NOSTR_RELAYS"), ",") {
		r = strings.TrimSpace(r)
		relay, err := nostr.RelayConnect(context.Background(), r)
		if err != nil {
			log.Printf("%v: %v", r, err)
			continue
		}
		status, err := relay.Publish(ctx, ev)
		relay.Close()
		if err == nil && status != nostr.PublishStatusFailed {
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
	var nsec string
	var ver bool

	flag.BoolVar(&skip, "skip", false, "Skip post")
	flag.StringVar(&dsn, "dsn", os.Getenv("FEED2NOSTR_DSN"), "Database source")
	flag.StringVar(&feedURL, "feed", "", "Feed URL")
	flag.StringVar(&format, "format", "{{.Title}}\n{{.Link}}", "Post Format")
	flag.StringVar(&nsec, "nsec", os.Getenv("FEED2NOSTR_NSEC"), "Nostr nsec")
	flag.BoolVar(&ver, "v", false, "show version")
	flag.Parse()

	if ver {
		fmt.Println(version)
		os.Exit(0)
	}

	t := template.Must(template.New("").Parse(format))

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
			GUID: item.GUID,
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
		if skip {
			log.Printf("%q", buf.String())
			continue
		}
		err = postNostr(nsec, buf.String())
		if err != nil {
			log.Println(err)
			continue
		}
	}
}
