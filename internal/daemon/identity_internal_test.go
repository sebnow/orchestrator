package daemon

import "testing"

func TestGivenAGitIdentityStringWhenParsedThenNameAndEmailOrAnErrorComeBack(t *testing.T) {
	cases := []struct {
		in, name, email string
		wantErr         bool
	}{
		{in: "Jane Doe <jane@example.com>", name: "Jane Doe", email: "jane@example.com"},
		{in: "jane@example.com", wantErr: true},
		{in: "<jane@example.com>", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			name, email, err := ParseGitIdentity(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseGitIdentity(%q) = %q, %q, nil; want an error", c.in, name, email)
				}
				return
			}
			if err != nil || name != c.name || email != c.email {
				t.Errorf("ParseGitIdentity(%q) = %q, %q, %v; want %q, %q, nil", c.in, name, email, err, c.name, c.email)
			}
		})
	}
}
