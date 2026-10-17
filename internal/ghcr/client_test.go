package ghcr

import (
	"slices"
	"testing"
)

func TestOrderChannelsFirst(t *testing.T) {
	tests := []struct {
		name string
		in   []ImageTag
		want []string
	}{
		{
			name: "nightly newest, latest in the middle",
			in: []ImageTag{
				{Tag: "f16a2fe", Nightly: true},
				{Tag: "6456f7b", Main: true},
				{Tag: "cf959b0"},
				{Tag: "eucalyptus", Latest: true},
				{Tag: "dogwood-r1"},
				{Tag: "dogwood"},
			},
			want: []string{"eucalyptus", "f16a2fe", "6456f7b", "cf959b0", "dogwood-r1", "dogwood"},
		},
		{
			name: "nightly and main on one build",
			in: []ImageTag{
				{Tag: "a"},
				{Tag: "b", Nightly: true, Main: true},
				{Tag: "c", Latest: true},
				{Tag: "d"},
			},
			want: []string{"c", "b", "a", "d"},
		},
		{
			name: "no nightly",
			in: []ImageTag{
				{Tag: "a"},
				{Tag: "b", Main: true},
				{Tag: "c"},
				{Tag: "d", Latest: true},
			},
			want: []string{"d", "b", "a", "c"},
		},
		{
			name: "no channels",
			in:   []ImageTag{{Tag: "a"}, {Tag: "b"}, {Tag: "c"}},
			want: []string{"a", "b", "c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orderChannelsFirst(tt.in)
			var got []string
			for _, r := range tt.in {
				got = append(got, r.Tag)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
