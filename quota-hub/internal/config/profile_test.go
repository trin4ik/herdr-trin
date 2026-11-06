package config

import "testing"

func TestAccountEmailPolicyIsExplicit(t *testing.T) {
	for _, auth := range []string{"bearer", "none"} {
		for _, publish := range []bool{false, true} {
			c := valid(t)
			c.HTTPAuth, c.PublishAccountEmail = auth, publish
			if auth == "none" {
				c.HTTPTokenEnv = ""
			}
			specs, err := c.Specs()
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range specs {
				if s.PublishAccountEmail != publish {
					t.Fatal("publication policy not propagated")
				}
			}
		}
	}
}
