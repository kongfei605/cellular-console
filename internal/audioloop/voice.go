package audioloop

import (
	"context"
	"errors"
	"github.com/kongfei605/ec20-toolbox/internal/voicecontrol"
	"github.com/kongfei605/ec20-toolbox/internal/voicepcm"
	"sync"
)

type VoiceProvider interface {
	Ready() error
	Prepare(context.Context, string) (voicepcm.Factory, func(), error)
}
type LiveVoice struct {
	Control                         voicecontrol.Client
	DeviceID, USBPath, FailoverFile string
}

func (v LiveVoice) Ready() error {
	if _, e := voicepcm.Discover("/sys", "/proc", v.USBPath); e != nil {
		return e
	}
	return voicecontrol.Ready()
}
func (v LiveVoice) Prepare(ctx context.Context, call string) (voicepcm.Factory, func(), error) {
	if !voicecontrol.CallPath.MatchString(call) {
		return nil, nil, errors.New("完整呼叫标识无效")
	}
	if e := v.Ready(); e != nil {
		return nil, nil, e
	}
	prepared, e := v.Control.Prepare(ctx, "active", call)
	if e != nil {
		return nil, nil, e
	}
	guard, e := voicepcm.NewSystemGuard(ctx, v.DeviceID, call, v.FailoverFile)
	if e != nil {
		return nil, nil, e
	}
	var once sync.Once
	close := func() { once.Do(func() { _ = guard.Close() }) }
	if guard.Owner() != prepared.Owner {
		close()
		return nil, nil, errors.New("通话服务已发生变化")
	}
	if e = guard.Check(ctx); e != nil {
		close()
		return nil, nil, e
	}
	return voicepcm.ALSAFactory(v.USBPath, guard.Check), close, nil
}
