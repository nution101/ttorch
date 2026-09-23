package scheduler

import "testing"

func TestSerializeOverlapFromEnvFollowsTheDaemonSetting(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{{"", false}, {"1", true}, {"true", true}, {"0", false}, {"nonsense", false}} {
		t.Setenv(envSerializeOverlap, tc.val)
		if got := SerializeOverlapFromEnv(); got != tc.want {
			t.Errorf("%s=%q: SerializeOverlapFromEnv() = %v, want %v", envSerializeOverlap, tc.val, got, tc.want)
		}
	}
}
