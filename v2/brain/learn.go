package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zephyrtronium/robot/v2/brain/userhash"
)

// Tuple is a single Markov chain tuple.
type Tuple struct {
	Prefix []string
	Suffix string
}

// MessageMeta holds metadata about a message.
type MessageMeta struct {
	// ID is a UUID for the message.
	ID uuid.UUID
	// User is an identifier for the user. It is obfuscated such that the user
	// cannot be identified and is not correlated between rooms.
	User userhash.Hash
	// Tag is a tag that should be associated with the message data.
	Tag string
	// Time is the time at which the message was sent.
	Time time.Time
}

// Learner records Markov chain tuples.
type Learner interface {
	// Order returns the number of elements in the prefix of a chain. It is
	// called once at the beginning of learning. The returned value must always
	// be at least 1.
	Order() int
	// Learn records a set of tuples. Each tuple prefix has length equal to the
	// result of Order. The tuples begin with empty strings in the prefix to
	// denote the start of the message and end with one empty suffix to denote
	// the end; all other tokens are non-empty. Each tuple's prefix has entropy
	// reduction transformations applied.
	Learn(ctx context.Context, meta *MessageMeta, tuples []Tuple) error
	// Forget removes a set of recorded tuples. The tuples provided are as for
	// Learn. If a tuple has been recorded multiple times, only the first
	// should be deleted. If a tuple has not been recorded, it should be
	// ignored.
	Forget(ctx context.Context, tag string, tuples []Tuple) error
}

// Learn records tokens into a Learner.
func Learn(ctx context.Context, l Learner, meta *MessageMeta, toks []string) error {
	n := l.Order()
	if n < 1 {
		panic(fmt.Errorf("order must be at least 1, got %d from %#v", n, l))
	}
	tt := tupleToks(make([]Tuple, 0, len(toks)+1), toks, n)
	return l.Learn(ctx, meta, tt)
}

// Forget removes tokens from a Learner.
func Forget(ctx context.Context, l Learner, tag string, toks []string) error {
	n := l.Order()
	if n < 1 {
		panic(fmt.Errorf("order must be at least 1, got %d from %#v", n, l))
	}
	tt := tupleToks(make([]Tuple, 0, len(toks)+1), toks, n)
	return l.Forget(ctx, tag, tt)
}

func tupleToks(tt []Tuple, toks []string, n int) []Tuple {
	p := Tuple{Prefix: make([]string, n)}
	for _, w := range toks {
		q := Tuple{Prefix: make([]string, n), Suffix: w}
		copy(q.Prefix, p.Prefix[1:])
		q.Prefix[n-1] = ReduceEntropy(p.Suffix)
		tt = append(tt, q)
		p = q
	}
	q := Tuple{Prefix: make([]string, n), Suffix: ""}
	copy(q.Prefix, p.Prefix[1:])
	q.Prefix[n-1] = ReduceEntropy(p.Suffix)
	tt = append(tt, q)
	return tt
}
