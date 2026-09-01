// Command hexlet-go-crawler walks a site and prints a JSON report about it.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/urfave/cli/v3"

	"code/crawler"
)

func main() {
	if err := newCommand().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:      "hexlet-go-crawler",
		Usage:     "analyze a website structure",
		ArgsUsage: "<url>",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "depth", Value: crawler.DefaultDepth, Usage: "crawl depth"},
			&cli.IntFlag{Name: "retries", Value: crawler.DefaultRetries, Usage: "number of retries for failed requests"},
			&cli.DurationFlag{Name: "delay", Usage: "delay between requests (example: 200ms, 1s)"},
			&cli.DurationFlag{Name: "timeout", Value: crawler.DefaultTimeout, Usage: "per-request timeout"},
			&cli.FloatFlag{Name: "rps", Usage: "limit requests per second (overrides delay)"},
			&cli.StringFlag{Name: "user-agent", Usage: "custom user agent"},
			&cli.IntFlag{Name: "workers", Value: crawler.DefaultConcurrency, Usage: "number of concurrent workers"},
		},
		Action: run,
	}
}

// run is deliberately forgiving: a missing URL or a site that will not answer
// is reported to the user, not turned into a non-zero exit code.
func run(ctx context.Context, cmd *cli.Command) error {
	url := cmd.Args().First()
	if url == "" {
		fmt.Fprintln(os.Stderr, "url is required, for example: hexlet-go-crawler https://example.com")
		return cli.ShowAppHelp(cmd)
	}

	timeout := cmd.Duration("timeout")

	report, err := crawler.Analyze(ctx, crawler.Options{
		URL:         url,
		Depth:       cmd.Int("depth"),
		Retries:     cmd.Int("retries"),
		Delay:       delayFrom(cmd.Float("rps"), cmd.Duration("delay")),
		Timeout:     timeout,
		UserAgent:   cmd.String("user-agent"),
		Concurrency: cmd.Int("workers"),
		IndentJSON:  true,
		HTTPClient:  &http.Client{Timeout: timeout},
	})
	if err != nil {
		if errors.Is(err, crawler.ErrNoURL) {
			fmt.Fprintln(os.Stderr, err)
			return nil
		}

		fmt.Fprintf(os.Stderr, "crawl failed: %v\n", err)

		return nil
	}

	fmt.Println(string(report))

	return nil
}

// delayFrom turns a requests-per-second limit into a pause between requests,
// because that is the single knob the crawler itself understands.
func delayFrom(rps float64, delay time.Duration) time.Duration {
	if rps > 0 {
		return time.Duration(float64(time.Second) / rps)
	}

	return delay
}
