package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"gitlab.com/zephyrtronium/pick"
	"gitlab.com/zephyrtronium/sq"
	"gitlab.com/zephyrtronium/tmi"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/sha3"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"

	"github.com/zephyrtronium/robot/auth"
	"github.com/zephyrtronium/robot/brain/kvbrain"
	"github.com/zephyrtronium/robot/channel"
	"github.com/zephyrtronium/robot/message"
	"github.com/zephyrtronium/robot/privacy"
)

// Load loads Robot from a TOML configuration.
func Load(ctx context.Context, r io.Reader) (*Config, *toml.MetaData, error) {
	var cfg Config
	md, err := toml.NewDecoder(r).Decode(&cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't decode config: %w", err)
	}
	expandcfg(&cfg, os.Getenv)
	return &cfg, &md, nil
}

// SetOwner sets owner metadata used in self-description commands.
func (robo *Robot) SetOwner(ownerName, ownerContact string) {
	robo.owner = ownerName
	robo.ownerContact = ownerContact
}

// SetSecrets loads the robot's fixed secret and initializes derived secrets.
func (robo *Robot) SetSecrets(file string) error {
	k, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("couldn't read secret key: %w", err)
	}
	uk := domainkey(make([]byte, 64), k, []byte("userhash"))
	tk := domainkey(make([]byte, auth.KeySize), k, []byte("oauth2.twitch"))
	robo.secrets = &keys{
		userhash: uk,
		twitch:   (*[32]byte)(tk),
	}
	return nil
}

// SetSources opens the brain and privacy list wrappers around the respective
// databases. Use [loadDBs] to open the databases themselves from DSNs.
func (robo *Robot) SetSources(ctx context.Context, brain *badger.DB, priv *sq.DB) error {
	var err error
	robo.brain = kvbrain.New(brain)
	robo.privacy, err = privacy.Open(ctx, priv)
	if err != nil {
		return fmt.Errorf("couldn't open privacy list: %w", err)
	}
	return nil
}

// SetTMI initializes the TMI client and Twitch channel configuration.
// It must be called after SetSecrets.
func (robo *Robot) SetTMI(ctx context.Context, cfg ClientCfg) error {
	cfg.endpoint = oauth2.Endpoint{
		DeviceAuthURL: "https://id.twitch.tv/oauth2/device",
		TokenURL:      "https://id.twitch.tv/oauth2/token",
	}
	send := make(chan *tmi.Message, 1)
	recv := make(chan *tmi.Message, 8) // 8 is enough for on-connect msgs
	client := &http.Client{Timeout: 30 * time.Second}
	tmi, err := loadClient(
		cfg,
		send,
		recv,
		func(c oauth2.Config, s auth.Storage) auth.TokenSource {
			return auth.DeviceCodeFlow(c, s, client, deviceCodePrompt)
		},
		*robo.secrets.twitch,
		"chat:read", "chat:edit",
	)
	if err != nil {
		return fmt.Errorf("couldn't load TMI client: %w", err)
	}
	robo.tmi = tmi
	return nil
}

// SetTwitchChannels initializes Twitch channel configuration.
// It must be called after SetTMI.
func (robo *Robot) SetTwitchChannels(ctx context.Context, global Global, channels map[string]*ChannelCfg) error {
	// TODO(zeph): we can convert this to a SetChannels, where it just adds the
	// channels for any given service
	for nm, ch := range channels {
		blk, err := regexp.Compile("(" + global.Block + ")|(" + ch.Block + ")")
		if err != nil {
			return fmt.Errorf("bad global or channel block expression for twitch.%s: %w", nm, err)
		}
		emotes := pick.New(pick.FromMap(mergemaps(global.Emotes, ch.Emotes)))
		var ign, mod map[string]bool
		for _, p := range ch.Privileges {
			// TODO(zeph): resolve user ids
			switch {
			case strings.EqualFold(p.Level, "ignore"):
				if ign == nil {
					ign = make(map[string]bool)
				}
				ign[p.ID] = true
			case strings.EqualFold(p.Level, "moderator"):
				if mod == nil {
					mod = make(map[string]bool)
				}
				mod[p.ID] = true
			}
		}
		for _, p := range ch.Channels {
			v := &channel.Channel{
				Name:      p,
				Learn:     ch.Learn,
				Send:      ch.Send,
				Block:     blk,
				Responses: ch.Responses,
				Rate:      rate.NewLimiter(rate.Every(fseconds(ch.Rate.Every)), ch.Rate.Num),
				Ignore:    ign,
				Mod:       mod,
				Memery:    channel.NewMemeDetector(ch.Copypasta.Need, fseconds(ch.Copypasta.Within)),
				Emotes:    emotes,
				// TODO(zeph): check channel online status for enabled
				Enabled: true,
			}
			v.Message = func(ctx context.Context, reply, text string) {
				msg := message.Format(reply, v.Name, "%s", text)
				robo.sendTMI(ctx, robo.tmi.send, msg)
			}
			robo.channels[p] = v
		}
	}
	return nil
}

func loadDBs(ctx context.Context, cfg DBCfg) (brain *badger.DB, priv *sq.DB, err error) {
	opts := badger.DefaultOptions(cfg.Brain)
	// TODO(zeph): logger?
	opts = opts.WithLogger(nil)
	opts = opts.WithCompression(options.None)
	opts = opts.WithBloomFalsePositive(0)
	brain, err = badger.Open(opts.FromSuperFlag(cfg.Brainflag))
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't open brain db: %w", err)
	}

	priv, err = sq.Open("sqlite3", cfg.Privacy)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't open privacy db: %w", err)
	}
	if err := priv.Ping(ctx); err != nil {
		return nil, nil, fmt.Errorf("couldn't connect to privacy db: %w", err)
	}

	return brain, priv, nil
}

func mergemaps(ms ...map[string]int) map[string]int {
	u := make(map[string]int)
	for _, m := range ms {
		for k, v := range m {
			u[k] += v
		}
	}
	return u
}

func fseconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// loadClient loads client configuration from unmarshaled TOML.
func loadClient[Send, Receive any](
	t ClientCfg,
	send chan Send,
	recv chan Receive,
	tokens func(oauth2.Config, auth.Storage) auth.TokenSource,
	key [auth.KeySize]byte,
	scopes ...string,
) (*client[Send, Receive], error) {
	secret, err := os.ReadFile(t.SecretFile)
	if err != nil {
		return nil, fmt.Errorf("couldn't read client secret: %w", err)
	}
	stor, err := auth.NewFileAt(t.TokenFile, key)
	if err != nil {
		return nil, fmt.Errorf("couldn't use refresh token storage: %w", err)
	}
	cfg := oauth2.Config{
		ClientID:     t.CID,
		ClientSecret: string(secret),
		Endpoint:     t.endpoint,
		RedirectURL:  t.RedirectURL,
		Scopes:       scopes,
	}
	return &client[Send, Receive]{
		send:   send,
		recv:   recv,
		me:     t.User,
		owner:  t.Owner.ID, // TODO(zeph): resolve username
		rate:   rate.NewLimiter(rate.Every(fseconds(t.Rate.Every)), t.Rate.Num),
		tokens: tokens(cfg, stor),
	}, nil
}

type keys struct {
	// userhash is the hasher for userhashes.
	userhash []byte
	// twitch is the key for Twitch OAuth2 token storage.
	twitch *[auth.KeySize]byte
}

// domainkey fills o with a key derived from k for the given domain. Panics if
// a key cannot be expanded.
func domainkey(o, k, domain []byte) []byte {
	kr := hkdf.Expand(sha3.New224, k, domain)
	if _, err := io.ReadFull(kr, o); err != nil {
		panic(err)
	}
	return o
}

// Config is the marshaled structure of Robot's configuration.
type Config struct {
	// SecretFile is the path to a file containing a secret key used to encrypt
	// durable secrets like OAuth2 refresh tokens as well as to create
	// userhashes.
	SecretFile string `toml:"secret"`
	// Owner is the table of metadata about the owner.
	Owner Owner `toml:"owner"`
	// DB is the table of database connection strings.
	DB DBCfg `toml:"db"`
	// Global is the table of global settings.
	Global Global `toml:"global"`
	// TMI is the configuration for connecting to Twitch chat.
	TMI ClientCfg `toml:"tmi"`
	// Twitch is the set of channel configurations for twitch. Each key
	// represents a group of one or more channels sharing a config.
	Twitch map[string]*ChannelCfg `toml:"twitch"`
}

// ChannelCfg is the configuration for a channel.
type ChannelCfg struct {
	// Channels is the list of channels using this config.
	Channels []string `toml:"channels"`
	// Learn is the tag used for learning from these channels.
	Learn string `toml:"learn"`
	// Send is the tag used for generating messages for these channels.
	Send string `toml:"send"`
	// Block is a regular expression of messages to ignore.
	Block string `toml:"block"`
	// Responses is the probability of generating a random message when
	// a non-command message is received.
	Responses float64 `toml:"responses"`
	// Rate is the rate limit for interactions.
	Rate Rate `toml:"rate"`
	// Copypasta is the configuration for copypasta.
	Copypasta Copypasta `toml:"copypasta"`
	// Emotes is the emotes and their weights for the channel.
	Emotes map[string]int `toml:"emotes"`
	// Privileges is the user access controls for the channel.
	Privileges []Privilege `toml:"privileges"`
}

// Global is the configuration for globally applied options.
type Global struct {
	// Block is a regular expression of messages to ignore everywhere.
	Block string `toml:"block"`
	// Emotes is the emotes and their weights to use everywhere.
	Emotes map[string]int `toml:"emotes"`
}

// Owner is metadata about the bot owner.
type Owner struct {
	// Name is the name of the owner. It does not need to be a username.
	Name string `toml:"name"`
	// Contact describes owner contact information.
	Contact string `toml:"contact"`
}

// ClientCfg is the configuration for connecting to an OAuth2 interface.
type ClientCfg struct {
	// User is robot's username. The interpretation of this is domain-specific.
	// On TMI, it is used to connect and to detect commands.
	User string `toml:"user"`
	// CID is the client ID.
	CID string `toml:"cid"`
	// SecretFile is the path to a file containing the client secret.
	SecretFile string `toml:"secret"`
	// RedirectURL is the redirect URL for OAuth2 flows. For clients that don't
	// use authorization code grant flow, it may be unused but still must match
	// the configuration on the platform.
	RedirectURL string `toml:"redirect"`
	// TokenFile is the path to a file in which the bot will persist its OAuth2
	// refresh token. It is encrypted with a key derived from the Config.Secret
	// key.
	TokenFile string `toml:"token"`
	// Owner is the user ID of the owner. The interpretation of this is
	// domain-specific.
	Owner Privilege `toml:"owner"`
	// Rate is the global rate limit for this client.
	Rate Rate `toml:"rate"`

	endpoint oauth2.Endpoint `toml:"-"`
}

type Privilege struct {
	// ID is the user ID.
	ID string `toml:"id"`
	// Name is the user name or login name (not display name).
	// If both ID and Name are provided, Name is generally ignored.
	Name string `toml:"name"`
	// Level is the access level granted to the user.
	// Valid values are the empty string as the default capability,
	// "ignore" to disable access to all commands including prompting,
	// or "moderator" to enable access to moderation commands.
	Level string `toml:"level"`
}

// DBCfg is the configuration of databases.
type DBCfg struct {
	Brain     string `toml:"brain"`
	Brainflag string `toml:"brainflag"`
	Privacy   string `toml:"privacy"`
}

// Rate is a rate limit configuration.
type Rate struct {
	Every float64 `toml:"every"`
	Num   int     `toml:"num"`
}

// Copypasta is a copypasta configuration.
type Copypasta struct {
	Need   int     `toml:"need"`
	Within float64 `toml:"within"`
}

func expandcfg(cfg *Config, expand func(s string) string) {
	fields := []*string{
		&cfg.SecretFile,
		&cfg.Owner.Name,
		&cfg.Owner.Contact,
		&cfg.DB.Brain,
		&cfg.DB.Privacy,
		&cfg.TMI.User,
		&cfg.TMI.CID,
		&cfg.TMI.SecretFile,
		&cfg.TMI.TokenFile,
		&cfg.TMI.Owner.Name,
		&cfg.TMI.Owner.ID,
	}
	for _, f := range fields {
		*f = os.Expand(*f, expand)
	}
	for _, v := range cfg.Twitch {
		for i, s := range v.Channels {
			v.Channels[i] = os.Expand(s, expand)
		}
		v.Learn = os.Expand(v.Learn, expand)
		v.Send = os.Expand(v.Send, expand)
	}
}
