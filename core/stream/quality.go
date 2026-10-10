package stream

import (
	"context"
	"strings"

	"github.com/navidrome/navidrome/model"
)

const MP3QualityBitrate = 192

// MakeQualityDecision keeps compatible direct play intact and normalizes every
// required transcode to the single supported output: CBR MP3 at 192 kbps.
func MakeQualityDecision(ctx context.Context, decider TranscodeDecider, mf *model.MediaFile, clientInfo *ClientInfo, opts TranscodeOptions) (*TranscodeDecision, error) {
	if clientInfo == nil {
		clientInfo = &ClientInfo{}
	}
	decision, err := decider.MakeDecision(ctx, mf, clientInfo, opts)
	if err != nil || decision == nil || decision.CanDirectPlay {
		return decision, err
	}
	if decision.CanTranscode && strings.EqualFold(decision.TargetFormat, "mp3") && decision.TargetBitrate == MP3QualityBitrate {
		return decision, nil
	}

	// Try direct play with the caller's real limits first. Once that fails,
	// transcode quality is a server policy, so lower client bitrate requests
	// must not lower the output below 192 kbps.
	forced := &ClientInfo{
		Name:                       clientInfo.Name,
		Platform:                   clientInfo.Platform,
		MaxAudioBitrate:            MP3QualityBitrate,
		MaxTranscodingAudioBitrate: MP3QualityBitrate,
		TranscodingProfiles: []Profile{{
			Container:  "mp3",
			AudioCodec: "mp3",
			Protocol:   ProtocolHTTP,
		}},
	}
	forcedDecision, err := decider.MakeDecision(ctx, mf, forced, TranscodeOptions{SkipProbe: true})
	if err != nil || forcedDecision == nil {
		return forcedDecision, err
	}
	forcedDecision.TranscodeReasons = mergeTranscodeReasons(decision.TranscodeReasons, forcedDecision.TranscodeReasons)
	if !forcedDecision.CanTranscode {
		return forcedDecision, nil
	}

	// A lossy source can be below 192 kbps. The output mode is still fixed CBR
	// MP3 192; encoding a higher bitrate does not make that source lossless.
	forcedDecision.TargetFormat = "mp3"
	forcedDecision.TargetBitrate = MP3QualityBitrate
	if forcedDecision.TranscodeStream == nil {
		source := forcedDecision.SourceStream
		forcedDecision.TranscodeStream = &Details{
			Container:  "mp3",
			Codec:      "mp3",
			Bitrate:    MP3QualityBitrate,
			SampleRate: source.SampleRate,
			Channels:   source.Channels,
			Duration:   source.Duration,
		}
	} else {
		target := *forcedDecision.TranscodeStream
		target.Container = "mp3"
		target.Codec = "mp3"
		target.Bitrate = MP3QualityBitrate
		target.BitDepth = 0
		target.IsLossless = false
		forcedDecision.TranscodeStream = &target
	}
	return forcedDecision, nil
}

func mergeTranscodeReasons(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var merged []string
	for _, group := range groups {
		for _, reason := range group {
			if _, exists := seen[reason]; exists {
				continue
			}
			seen[reason] = struct{}{}
			merged = append(merged, reason)
		}
	}
	return merged
}
