package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"

	"github.com/zephyrtronium/robot/brain"
	"github.com/zephyrtronium/robot/brain/kvbrain"
	"github.com/zephyrtronium/robot/brain/sqlbrain"
)

var app = cli.Command{
	Name:  "robot",
	Usage: "Markov chain chat bot",

	Flags: []cli.Flag{
		&flagConfig,
		&flagLog,
		&flagLogFormat,
	},
	Commands: []*cli.Command{
		{
			Name:    "speak",
			Aliases: []string{"talk", "generate", "say"},
			Usage:   "Generate messages without serving",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "tag",
					Usage:    "Tag from which to generate messages",
					Required: true,
				},
				&cli.IntFlag{
					Name:  "n",
					Usage: "Number of messages to generate",
					Value: 10,
				},
				&cli.StringFlag{
					Name:  "prompt",
					Usage: "Prompt to use for all generated messages",
				},
				&cli.BoolFlag{
					Name:  "trace",
					Usage: "Print ID traces with messages",
				},
			},
			Action: cliSpeak,
		},
	},
	Action: cliRun,

	Authors: []any{
		"Branden J Brown  @zephyrtronium",
	},
	Copyright: "Copyright 2024 Branden J Brown",
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	go func() {
		<-ctx.Done()
		stop()
	}()
	err := app.Run(ctx, os.Args)
	if err != nil {
		fmt.Println(err)
	}
}

func cliRun(ctx context.Context, cmd *cli.Command) error {
	slog.SetDefault(loggerFromFlags(cmd))
	r, err := os.Open(cmd.String("config"))
	if err != nil {
		return fmt.Errorf("couldn't open config file: %w", err)
	}
	cfg, md, err := Load(ctx, r)
	if err != nil {
		return fmt.Errorf("couldn't load config: %w", err)
	}
	r.Close()
	robo := New(runtime.GOMAXPROCS(0))
	robo.SetOwner(cfg.Owner.Name, cfg.Owner.Contact)
	if err := robo.SetSecrets(cfg.SecretFile); err != nil {
		return err
	}
	kv, sql, priv, spoke, err := loadDBs(ctx, cfg.DB)
	if err != nil {
		return err
	}
	if err := robo.SetSources(ctx, kv, sql, priv, spoke); err != nil {
		return err
	}
	if md.IsDefined("tmi") {
		if err := robo.InitTwitch(ctx, cfg.TMI); err != nil {
			return err
		}
		if err := robo.InitTwitchUsers(ctx, &cfg.TMI.Owner, cfg.Global.Privileges.Twitch, cfg.Twitch); err != nil {
			return err
		}
		if err := robo.SetTwitchChannels(ctx, cfg.Global, cfg.Twitch); err != nil {
			return err
		}
	}
	return robo.Run(ctx)
}

func cliSpeak(ctx context.Context, cmd *cli.Command) error {
	slog.SetDefault(loggerFromFlags(cmd))
	r, err := os.Open(cmd.String("config"))
	if err != nil {
		return fmt.Errorf("couldn't open config file: %w", err)
	}
	cfg, _, err := Load(ctx, r)
	if err != nil {
		return fmt.Errorf("couldn't load config: %w", err)
	}
	r.Close()
	kv, sql, _, _, err := loadDBs(ctx, cfg.DB)
	if err != nil {
		return err
	}
	var br brain.Brain
	if sql == nil {
		if kv == nil {
			panic("robot: no brain")
		}
		br = kvbrain.New(kv)
		defer kv.Close()
	} else {
		br, err = sqlbrain.Open(ctx, sql)
		defer sql.Close()
	}
	if err != nil {
		return fmt.Errorf("couldn't open brain: %w", err)
	}
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(runtime.GOMAXPROCS(0))
	tag := cmd.String("tag")
	trace := cmd.Bool("trace")
	prompt := cmd.String("prompt")
	for range cmd.Int("n") {
		group.Go(func() error {
			m, tr, err := brain.Speak(ctx, br, tag, prompt)
			if err != nil {
				return err
			}
			a := []any{m}
			if trace {
				a = append(a, tr)
			}
			fmt.Println(a...)
			return nil
		})
	}
	return group.Wait()
}

var (
	flagConfig = cli.StringFlag{
		Name:       "config",
		Required:   true,
		Usage:      "TOML config file",
		Persistent: true,
		Action: func(ctx context.Context, cmd *cli.Command, s string) error {
			i, err := os.Stat(s)
			if err != nil {
				return err
			}
			if !i.Mode().IsRegular() {
				return errors.New("config must be a regular file")
			}
			return nil
		},
	}

	flagLog = cli.StringFlag{
		Name:       "log",
		Usage:      "Logging level, one of debug, info, warn, error",
		Value:      "info",
		Persistent: true,
		Action: func(ctx context.Context, c *cli.Command, s string) error {
			var l slog.Level
			return l.UnmarshalText([]byte(s))
		},
	}

	flagLogFormat = cli.StringFlag{
		Name:       "log-format",
		Usage:      "Logging format, either text or json",
		Value:      "text",
		Persistent: true,
		Action: func(ctx context.Context, c *cli.Command, s string) error {
			switch strings.ToLower(s) {
			case "text", "json":
				return nil
			default:
				return errors.New("unknown logging format")
			}
		},
	}
)

func loggerFromFlags(cmd *cli.Command) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(cmd.String("log"))); err != nil {
		panic(err)
	}
	var h slog.Handler
	switch strings.ToLower(cmd.String("log-format")) {
	case "text":
		h = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})
	case "json":
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})
	}
	return slog.New(h)
}
