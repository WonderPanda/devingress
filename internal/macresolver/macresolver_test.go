package macresolver

import (
	"slices"
	"testing"

	"github.com/WonderPanda/devingress/internal/service"
)

func TestFile(t *testing.T) {
	got, err := File("127.0.0.1:15353", "example.dev")
	want := "# Written by devingress: routes example.dev to serve's DNS listener.\nnameserver 127.0.0.1\nport 15353\n"
	if err != nil || got != want {
		t.Errorf("File: %q, %v; want %q", got, err, want)
	}
	if !Ours(got) {
		t.Error("Ours(File(...)) = false")
	}
	if got, err := File("[::1]:53", "example.dev"); err != nil || got != "# Written by devingress: routes example.dev to serve's DNS listener.\nnameserver ::1\nport 53\n" {
		t.Errorf("IPv6: %q, %v", got, err)
	}
	if _, err := File("127.0.0.1", "example.dev"); err == nil {
		t.Error("dns_listen without a port: no error")
	}
	if Ours("nameserver 10.0.0.1\n") || Ours("# Written by devingressfoo\n") {
		t.Error("Ours claims a foreign file")
	}
}

func lines(steps []service.Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.String())
	}
	return out
}

func TestSteps(t *testing.T) {
	steps := Setup("example.dev", "FILE")
	want := []string{"sudo mkdir -p /etc/resolver", "sudo tee /etc/resolver/example.dev", "sudo killall -HUP mDNSResponder"}
	if got := lines(steps); !slices.Equal(got, want) || steps[1].Stdin != "FILE" {
		t.Errorf("setup: %q, want %q", got, want)
	}
	want = []string{"sudo rm -f /etc/resolver/example.dev", "sudo killall -HUP mDNSResponder"}
	if got := lines(Remove("example.dev")); !slices.Equal(got, want) {
		t.Errorf("remove: %q, want %q", got, want)
	}
}
