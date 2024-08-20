package command

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/zephyrtronium/robot/brain"
)

func speakCmd(ctx context.Context, robo *Robot, call *Invocation, effect string) string {
	m, trace, err := brain.Speak(ctx, robo.Brain, call.Channel.Send, call.Args["prompt"])
	if err != nil {
		robo.Log.ErrorContext(ctx, "couldn't speak", "err", err.Error())
		return ""
	}
	e := call.Channel.Emotes.Pick(rand.Uint32())
	s := m + " " + e
	if err := robo.Spoken.Record(ctx, call.Channel.Send, m, trace, call.Message.Time(), 0, e, effect); err != nil {
		robo.Log.ErrorContext(ctx, "couldn't record trace", slog.Any("err", err))
		return ""
	}
	if call.Channel.Block.MatchString(s) {
		robo.Log.WarnContext(ctx, "generated blocked message",
			slog.String("in", call.Channel.Name),
			slog.String("text", m),
			slog.String("emote", e),
		)
		return ""
	}
	t := time.Now()
	r := call.Channel.Rate.ReserveN(t, 1)
	if d := r.DelayFrom(t); d > 0 {
		robo.Log.InfoContext(ctx, "won't speak; rate limited",
			slog.String("action", "command"),
			slog.String("in", call.Channel.Name),
			slog.String("delay", d.String()),
		)
		r.CancelAt(t)
		return ""
	}
	slog.InfoContext(ctx, "speak", "in", call.Channel.Name, "text", m, "emote", e)
	return m + " " + e
}

func Speak(ctx context.Context, robo *Robot, call *Invocation) {
	u := speakCmd(ctx, robo, call, "")
	if u == "" {
		return
	}
	u = lenlimit(u, 450)
	call.Channel.Message(ctx, "", u)
}

func OwO(ctx context.Context, robo *Robot, call *Invocation) {
	u := speakCmd(ctx, robo, call, "OwO")
	if u == "" {
		return
	}
	u = lenlimit(owoize(u), 450)
	call.Channel.Message(ctx, "", u)
}

func AAAAA(ctx context.Context, robo *Robot, call *Invocation) {
	u := speakCmd(ctx, robo, call, "AAAAA")
	if u == "" {
		return
	}
	u = lenlimit(aaaaaize(u), 40)
	call.Channel.Message(ctx, "", u)
}

var (
	_ Func = Speak
	_ Func = OwO
	_ Func = AAAAA
)
