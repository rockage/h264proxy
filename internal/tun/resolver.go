package tun

import (
	"context"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"net"
)

// PacketResolver provides the same DoH, IPv4 policy and cache as the CLI TUN
// frontend, without listening on sockets or changing system network settings.
type PacketResolver struct{ proxy *dnsProxy }

func NewPacketResolver(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), ipv6 bool, log *logging.Logger) *PacketResolver {
	return &PacketResolver{newDNSProxy(ctx, dial, ipv6, log)}
}
func (r *PacketResolver) Resolve(ctx context.Context, query []byte, udp bool) []byte {
	return r.proxy.answer(ctx, query, udp)
}
func (r *PacketResolver) Check(ctx context.Context) error { return r.proxy.check(ctx) }
func (r *PacketResolver) Close()                          { r.proxy.Close() }
