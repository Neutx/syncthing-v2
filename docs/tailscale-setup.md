# Setting up Tailscale

SyncThing V2 uses [Tailscale](https://tailscale.com/) to find your other computers and to reach them. Tailscale gives every device a private `100.x.y.z` address that works from anywhere, and it ties that address to a signed-in user. SyncThing V2 only reads Tailscale's status (`tailscale status --json`). It never changes your Tailscale settings and never installs Tailscale without asking.

## 1. Install and sign in on every computer

| OS | Get Tailscale |
|---|---|
| Windows | [tailscale.com/download/windows](https://tailscale.com/download/windows). The SyncThing V2 dashboard also offers **Install with winget** (`winget install -e --id Tailscale.Tailscale`), which runs only when you click it. |
| macOS | The Mac App Store or [tailscale.com/download/mac](https://tailscale.com/download/mac). Homebrew's `tailscale` works too. |
| Linux | [tailscale.com/download/linux](https://tailscale.com/download/linux), then `sudo tailscale up` |

Sign in on each computer. Check that Tailscale shows **Connected** and that the other computers appear in its device list. `stv2 doctor` reports [TS001](troubleshooting.md#ts001) if the Tailscale command-line tool is missing, and [TS002](troubleshooting.md#ts002) if Tailscale is not connected.

## 2. Same account, or other people's devices

- **Your devices** are computers signed in with the same Tailscale user. SyncThing V2 lists them first, under "Your devices". This is the normal case.
- **Other people's devices** are computers of other users in the same tailnet, such as family members or colleagues. They are listed separately, with an "Other owner" badge. A pairing request from such a device shows the owner's login and is marked "only accept if you know this person". Nothing is ever accepted without a click on that computer.
- **Not listed at all:** devices shared into your tailnet from another tailnet (node sharing), tagged devices (servers, usually managed by an admin), phones and tablets, and devices that are offline.

## 3. Let port 22000 through the tailnet policy

Syncthing talks to other devices on port **22000** (TCP, plus UDP for QUIC). Tailscale's default policy allows all traffic between your own devices, so most people need no change here.

If your tailnet has a custom access policy, allow 22000 between each user's own devices. With **grants** (the current policy syntax), add this to the tailnet policy file in the Tailscale admin console:

```json
{
  "grants": [
    {
      "src": ["autogroup:member"],
      "dst": ["autogroup:self"],
      "ip": ["tcp:22000", "udp:22000"]
    }
  ]
}
```

The same rule in the older **`acls`** syntax:

```json
{
  "acls": [
    {
      "action": "accept",
      "src": ["autogroup:member"],
      "dst": ["autogroup:self:22000"]
    }
  ]
}
```

`autogroup:self` means "devices of the same user", so this rule does not open anything between different people. To sync with other people's devices, add a rule that names them explicitly. When the port is blocked, `stv2 doctor` reports [NET001](troubleshooting.md#net001), and the Pair view shows the device as "Syncthing not reachable".

## 4. Shields Up and "Allow incoming connections"

Tailscale's **Shields Up** setting (on macOS and Windows, "Allow incoming connections" turned off) blocks every incoming connection to that computer, including Syncthing's.

Pairing through SyncThing V2 needs **both** computers to accept connections on port 22000. The computer where you click **Pair** knocks on the other one to find it, and the computer where you click **Accept** knocks back to check the request. A computer with Shields Up therefore shows up in the Pair view as not having Syncthing, and a request it sends is never shown on the other side. Turn Shields Up off on both computers while you pair, or [pair by hand](pairing.md#manual-fallback-pairing-in-syncthings-web-ui).

After pairing, one computer with Shields Up is fine for sync, because each side stores the other's address and either side may connect. If both computers have Shields Up on, they cannot connect.

## 5. MagicDNS is not required

SyncThing V2 configures Syncthing with the tailnet IPv4 addresses (`tcp://100.x.y.z:22000` and `quic://100.x.y.z:22000`). It does not use MagicDNS names, so pairing works with MagicDNS on or off. After the first successful connection, SyncThing V2 also adds Syncthing's `dynamic` address. The devices can then keep finding each other on the local network, or through Syncthing's discovery in the hybrid profile, if a tailnet address ever changes.

## 6. Transport profiles

A transport profile decides whether Syncthing may also use its public infrastructure. The profiles are global discovery servers, relay servers and NAT traversal.

| Profile | Settings | When it is used |
|---|---|---|
| **Tailnet only** (`tailnet`) | global discovery, relays and NAT traversal off; local discovery on; listen on the default addresses | Applied once, automatically, to a Syncthing that SyncThing V2 set up itself |
| **Hybrid** (`hybrid`) | global discovery, relays and NAT traversal on (Syncthing's defaults) | Your choice |

**The trade-off.** With **Tailnet only**, your devices never announce themselves to public discovery servers, and your data never goes through public relays. However, sync only works while Tailscale is connected on both computers, or while both are on the same local network. **Hybrid** keeps Syncthing's ability to connect without Tailscale, at the cost of using the public discovery and relay servers. Data is always end-to-end encrypted by Syncthing, in either profile.

A Syncthing that you already had keeps its settings. SyncThing V2 changes the profile only when you ask:

- in the dashboard, under **Settings → Transport profile** (it asks for confirmation), or
- from a terminal: `stv2 profile tailnet` or `stv2 profile hybrid` (add `--yes` to skip the question).

## Exit nodes and subnet routers

Using an exit node or advertising subnet routes does not affect pairing. A subnet router can make traffic appear to come from a different address. SyncThing V2 therefore checks that the device at a request's tailnet address really holds the requesting device ID before it shows the request. If it does not, the request is ignored ("identity mismatch") and left to Syncthing's own web UI. See [pairing.md](pairing.md) and [security.md](security.md).
