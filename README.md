# feed2nostr

Post event via RSS feed.

## Usage

```
feed2nostr -dsn $DATABASE_URL \
    -feed https://www.reddit.com/r/golang.rss \
    -format '{{.Title}}{{"\n"}}{{.Link}} #golang_news'
```

Or kubernetes cronjob.

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: golang_news-bot
spec:
  schedule: '0 * * * *'
  successfulJobsHistoryLimit: 1
  failedJobsHistoryLimit: 1
  jobTemplate:
    spec:
      backoffLimit: 1
      template:
        spec:
          containers:
          - name: golang_news-bot
            image: mattn/feed2nostr
            imagePullPolicy: IfNotPresent
            #imagePullPolicy: Always
            command: ["/go/bin/feed2nostr"]
            args:
            - '-feed'
            - 'https://www.reddit.com/r/golang.rss'
            - '-format'
            - '{{.Title}}{{\"\n\"}}{{.Link}} #vimeditor'
            env:
            - name: FEED2NOSTR_NSEC
              valueFrom:
                configMapKeyRef:
                  name: nsec1xxxxxx
                  key: feed2nostr-nsec
            - name: FEED2NOSTR_RELAYS
              valueFrom:
                configMapKeyRef:
                  name: golang_news-bot
                  key: feed2nostr-relays
            - name: FEED2NOSTR_DSN
              valueFrom:
                configMapKeyRef:
                  name: golang_news-bot
                  key: feed2nostr-dsn
          restartPolicy: Never
```

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: golang-news-bot
data:
  feed2nostr-nsec: 'XXXXXXXXXXXXXXXXXXXXXX'
  feed2nostr-relays: 'wss://server1:7447,wss://server2:8080'
  feed2nostr-dsn: 'postgres://user:password@server/database'
```

## Installation

```
$ go install github.com/mattn/feed2nostr@latest
```

Or use `mattn/feed2nostr` for docker image.

## License

MIT

## Author

Yasuihro Matsumoto (a.k.a. mattn)
