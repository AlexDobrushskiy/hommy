package hub

import "testing"

func TestParseTarget(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Target
	}{
		{"vault@mac1", Target{Kind: KindVault, Host: "mac1"}},
		{" p@mac1:~/agent-briefs ", Target{Kind: KindP, Host: "mac1", Dir: "~/agent-briefs"}},
		{"p@mac2:/Users/tester/x y", Target{Kind: KindP, Host: "mac2", Dir: "/Users/tester/x y"}},
		{"session@mac2:D5E7A2D3-0F1B-4C2D-9E8F-112233445566", Target{Kind: KindSession, Host: "mac2", Dir: "D5E7A2D3-0F1B-4C2D-9E8F-112233445566"}},
		{"new@mac1:~/agent-briefs:interactive", Target{Kind: KindNew, Host: "mac1", Dir: "~/agent-briefs", Mode: ModeInteractive}},
		{"new@mac3:/home/ownerUser/a:b:-p", Target{Kind: KindNew, Host: "mac3", Dir: "/home/ownerUser/a:b", Mode: ModeP}},
	} {
		got, err := ParseTarget(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q -> %+v, %v", c.in, got, err)
		}
		if again, _ := ParseTarget(got.String()); again != got {
			t.Errorf("round trip %q", got.String())
		}
	}
	for _, bad := range []string{"", "mac1", "vault@", "vault@Mac1!", "p@mac1", "p@mac1:", "p@mac1:rel/dir", "x@mac1", "p@:/tmp",
		"vault@mac1:x", "session@mac2:", "session@mac2:a b", "session@mac2:../x", "new@mac1:~/x", "new@mac1:~/x:batch", "new@mac1:rel:-p"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
