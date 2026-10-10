package stream

import (
	"context"
	"testing"

	"github.com/navidrome/navidrome/model"
)

type qualityTestDecider struct {
	decisions []*TranscodeDecision
	clients   []*ClientInfo
}

func (d *qualityTestDecider) MakeDecision(_ context.Context, _ *model.MediaFile, client *ClientInfo, _ TranscodeOptions) (*TranscodeDecision, error) {
	d.clients = append(d.clients, client)
	index := len(d.clients) - 1
	if index >= len(d.decisions) {
		return &TranscodeDecision{}, nil
	}
	return d.decisions[index], nil
}

func (*qualityTestDecider) CreateTranscodeParams(*TranscodeDecision) (string, error) { return "", nil }
func (*qualityTestDecider) ResolveRequestFromToken(context.Context, string, *model.MediaFile, int) (Request, error) {
	return Request{}, nil
}
func (*qualityTestDecider) ResolveRequest(context.Context, *model.MediaFile, string, int, int) Request {
	return Request{}
}
func (*qualityTestDecider) ResolveClientRequest(context.Context, *model.MediaFile, *ClientInfo, int) Request {
	return Request{}
}

func TestMakeQualityDecisionPreservesCompatibleDirectPlay(t *testing.T) {
	direct := &TranscodeDecision{CanDirectPlay: true}
	d := &qualityTestDecider{decisions: []*TranscodeDecision{direct}}
	result, err := MakeQualityDecision(context.Background(), d, &model.MediaFile{}, &ClientInfo{Name: "player"}, TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result != direct || len(d.clients) != 1 {
		t.Fatalf("direct play result=%+v calls=%d, want original result and no fallback", result, len(d.clients))
	}
}

func TestMakeQualityDecisionNormalizesRequiredTranscode(t *testing.T) {
	initial := &TranscodeDecision{TranscodeReasons: []string{"requested codec unsupported"}, TargetFormat: "opus", TargetBitrate: 128}
	fallback := &TranscodeDecision{
		CanTranscode:    true,
		TargetFormat:    "mp3",
		TargetBitrate:   96,
		SourceStream:    Details{Container: "aac", Codec: "aac", Bitrate: 96, Channels: 2, SampleRate: 44100},
		TranscodeStream: &Details{Container: "mp3", Codec: "mp3", Bitrate: 96, Channels: 2, SampleRate: 44100},
	}
	d := &qualityTestDecider{decisions: []*TranscodeDecision{initial, fallback}}
	result, err := MakeQualityDecision(context.Background(), d, &model.MediaFile{}, &ClientInfo{
		Name: "player", MaxAudioBitrate: 64,
		TranscodingProfiles: []Profile{{Container: "opus", AudioCodec: "opus", Protocol: ProtocolHTTP}},
	}, TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.CanTranscode || result.TargetFormat != "mp3" || result.TargetBitrate != MP3QualityBitrate {
		t.Fatalf("normalized decision = %+v", result)
	}
	if result.TranscodeStream == nil || result.TranscodeStream.Container != "mp3" || result.TranscodeStream.Codec != "mp3" || result.TranscodeStream.Bitrate != MP3QualityBitrate {
		t.Fatalf("normalized output details = %+v", result.TranscodeStream)
	}
	if len(result.TranscodeReasons) != 1 || result.TranscodeReasons[0] != "requested codec unsupported" {
		t.Fatalf("transcode reasons = %v", result.TranscodeReasons)
	}
	forced := d.clients[1]
	if forced.Name != "player" || forced.MaxAudioBitrate != 192 || forced.MaxTranscodingAudioBitrate != 192 {
		t.Fatalf("fallback client limits = %+v", forced)
	}
	if len(forced.TranscodingProfiles) != 1 || forced.TranscodingProfiles[0].Container != "mp3" {
		t.Fatalf("fallback profiles = %+v", forced.TranscodingProfiles)
	}
}

func TestMakeQualityDecisionRetainsAlreadyNormalizedTranscode(t *testing.T) {
	decision := &TranscodeDecision{CanTranscode: true, TargetFormat: "MP3", TargetBitrate: MP3QualityBitrate}
	d := &qualityTestDecider{decisions: []*TranscodeDecision{decision}}
	result, err := MakeQualityDecision(context.Background(), d, &model.MediaFile{}, nil, TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result != decision || len(d.clients) != 1 {
		t.Fatalf("already-normalized result=%+v calls=%d", result, len(d.clients))
	}
}
