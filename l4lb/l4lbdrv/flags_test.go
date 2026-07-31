package l4lbdrv

import "testing"

func TestConfigFlags(t *testing.T) {
	if f := configFlags(&DynamicConfig{}); f != 0 {
		t.Fatalf("default flags = %#x, want 0", f)
	}
	if f := configFlags(&DynamicConfig{IcmpEcho: true}); f&LbConfigFlagIcmpEchoReply == 0 {
		t.Fatalf("IcmpEcho=true must set LbConfigFlagIcmpEchoReply, flags=%#x", f)
	}
	// The flag bit must match the C #define LB_CONFIG_FLAG_ICMP_ECHO_REPLY (1<<2).
	if LbConfigFlagIcmpEchoReply != 1<<2 {
		t.Fatalf("LbConfigFlagIcmpEchoReply = %#x, want %#x (must match lb.c)", LbConfigFlagIcmpEchoReply, 1<<2)
	}
}
