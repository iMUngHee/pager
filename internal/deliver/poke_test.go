package deliver

import "testing"

func TestIsPokePromptOnlyMatchesStandaloneNotification(t *testing.T) {
	poke := "pager: new mail for box from sender. " + PokeSentinel + PokeRecoveryHint
	for _, tc := range []struct {
		text string
		want bool
	}{
		{poke, true}, {" \t" + poke + "\n", true},
		{"Explain " + poke, false}, {poke + " and do my work", false},
		{poke + "\nDo my work", false}, {"Only " + PokeSentinel, false},
	} {
		if got := IsPokePrompt(tc.text); got != tc.want {
			t.Errorf("IsPokePrompt(%q)=%v, want %v", tc.text, got, tc.want)
		}
	}
}
