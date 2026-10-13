package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestHDRMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, metadata string
		hdr            bool
	}{
		{"PQ", `"color_transfer":"smpte2084"`, true},
		{"HLG", `"color_transfer":"arib-std-b67"`, true},
		{"Dolby Vision", `"side_data_list":[{"side_data_type":"DOVI configuration record"}]`, true},
		{"HDR10+", `"side_data_list":[{"side_data_type":"HDR Dynamic Metadata SMPTE2094-40 (HDR10+)"}]`, true},
		{"mastering", `"side_data_list":[{"side_data_type":"Mastering display metadata"}]`, true},
		{"light level", `"side_data_list":[{"side_data_type":"Content light level metadata"}]`, true},
		{"SDR", `"color_transfer":"bt709"`, false},
		{"unknown", `"color_transfer":"unknown"`, false},
		{"BT2020", `"color_primaries":"bt2020","color_space":"bt2020nc"`, false},
		{"rotation", `"side_data_list":[{"side_data_type":"Display Matrix","rotation":90}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fmt.Sprintf(`{"format":{"duration":"3"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","pix_fmt":"yuv420p10le","avg_frame_rate":"24/1",%s}],"pixel_formats":[{"name":"yuv420p10le","flags":{"alpha":0}}]}`, tc.metadata)
			info, err := parseMediaInfo("source.mkv", []byte(data))
			if err != nil {
				t.Fatal(err)
			}
			if (info.HDR != "") != tc.hdr {
				t.Fatalf("HDR=%q", info.HDR)
			}
			if err := validateSourceVideo(info); (err != nil) != tc.hdr {
				t.Fatalf("validation: %v", err)
			}
			reason, err := parseInitialHDR([]byte(fmt.Sprintf(`{"frames":[{%s}]}`, tc.metadata)))
			if err != nil || (reason != "") != tc.hdr {
				t.Fatalf("decoded metadata=%q, %v", reason, err)
			}
			if tc.hdr {
				for _, opts := range []EncodeOptions{{}, {CRFSet: true, CRF: 32}, {FallbackCRFSet: true, FallbackCRF: 32}, {GroupCRF: true}, {ProbeOptions: ProbeOptions{ForceVideo: true}}} {
					if err := encodeOneWithCRF(context.Background(), opts, eligibleInput{Info: info}, 32, nil); err == nil || !strings.Contains(err.Error(), "HDR") {
						t.Fatalf("bypassed HDR guard: %v", err)
					}
				}
			}
		})
	}
	for _, data := range []string{`{}`, `{"frames":[]}`, `invalid`} {
		if _, err := parseInitialHDR([]byte(data)); err == nil {
			t.Fatalf("accepted missing metadata: %s", data)
		}
	}
}
