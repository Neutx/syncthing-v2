# Pairing computers

Syncthing only syncs between devices that know each other's **device ID**, a long code like `ABCDEFG-HIJKLMN-…`. Normally you copy that code from one computer to the other by hand. SyncThing V2 does this for you over Tailscale, with a click on each computer.

You need:

- Tailscale connected on both computers ([tailscale-setup.md](tailscale-setup.md))
- SyncThing V2 running on both computers
- Syncthing listening on its default port, 22000, on both computers. This is the default. See [non-default ports](#non-default-ports) otherwise.

## The flow, in plain words

Say you are pairing your **desktop** with your **laptop**.

1. **Find.** On the desktop, open the dashboard and go to **Pair devices** (the link icon at the top, or **Pair Devices…** in the tray menu). SyncThing V2 asks Tailscale which of your computers are online. It then knocks on port 22000 of each one and asks its Syncthing for its device ID. This knock stops before Syncthing would notice a visitor, so it leaves no trace or request on the other computer. The search runs when SyncThing V2 starts, when you open the Pair view, and when you click **Refresh**. It never runs in the background.
2. **Pair.** Click **Pair** next to the laptop. The desktop's Syncthing now knows the laptop by its device ID and its tailnet address, and it tries to connect.
3. **Accept.** The laptop's Syncthing sees an unknown device knocking and holds it as a request. SyncThing V2 on the laptop checks three things before it shows the request:
   - the request comes from a tailnet address
   - Tailscale knows which computer has that address
   - the Syncthing at that address really has the device ID that is asking

   If all three hold, the laptop shows a notification and a banner: "**desktop** wants to sync with this computer · Windows · ID `ABCDEFG`". Click **Accept**. If you clicked **Pair** on both computers, they connect straight away and no request is needed.
4. **Share a folder.** On the desktop, click **Share a folder…** in the Pair view. The first time, the Pair view goes there by itself. Pick one of your existing Syncthing folders, or **New folder…** to choose a directory (the default is a `Sync` folder in your home directory). Then tick the devices to share it with.
5. **Accept the folder.** The laptop shows "**desktop** shares "Photos"". Choose one of these:
   - **Accept** puts it in `~/Sync/Photos` (on Windows `%USERPROFILE%\Sync\Photos`). The name is cleaned of unsafe characters, and ` (2)`, ` (3)` and so on is added if that folder already exists and is not empty.
   - **Choose location…** lets you pick the folder yourself.
   - **Decline** removes the offer.

Syncthing now keeps the folder in sync in both directions. After the first connection, SyncThing V2 adds Syncthing's `dynamic` address for the new device. The two computers can then also find each other directly on a local network.

Nothing is ever accepted automatically: no device and no folder. Folder offers from devices you have not paired are ignored. Requests that fail any of the three checks are not shown by SyncThing V2. They stay visible in Syncthing's own web UI, and the application log records why each one was ignored.

From a terminal, `stv2 pair --list` prints the same list the Pair view shows. Add `--json` for JSON. Listing never changes anything.

## What each status means

| Pair view shows | Meaning | What to do |
|---|---|---|
| **Pair** button | The computer runs Syncthing on port 22000, and SyncThing V2 read its device ID. | Click **Pair**. |
| **Paired** | This device ID is already in your Syncthing configuration. | Nothing. Share folders with it. |
| "Syncthing not reachable — is SyncThing V2 running there? Check ACLs/firewall" | The connection to port 22000 was refused: nothing listens there, or a firewall rejects it. | Install or start SyncThing V2 on that computer. Check the [tailnet policy](tailscale-setup.md#3-let-port-22000-through-the-tailnet-policy) and its firewall. |
| "Install SyncThing V2 on this device (or it uses a non-default port)" | No answer, or no Syncthing answer, within 3 seconds. | Install SyncThing V2 there, or see [non-default ports](#non-default-ports). The computer may also have Tailscale's Shields Up on. Pair from its side instead. |
| **Other owner** badge | The computer belongs to another user of your tailnet. | Pair only if you know this person. Their requests show their Tailscale login. |
| Your own computer | Not shown. | |

A pairing request is labelled **Owned by *login* — only accept if you know this person** when it comes from another user's computer.

If you **Decline** a request, SyncThing V2 does not show requests from that device again. To pair with it later, click **Pair** on it from this computer.

## Manual fallback: pairing in Syncthing's web UI

When pairing through SyncThing V2 is not possible (Tailscale is not available, a policy blocks port 22000 in one direction, or a device uses another port), pair in Syncthing's own web UI. This is the usual Syncthing way:

1. On each computer, open **Open Web UI** from the SyncThing V2 tray menu. Then choose **Actions → Show ID** and copy the device ID.
2. On one computer, click **Add Remote Device**, paste the other computer's ID and give it a name. Under **Advanced → Addresses**, enter `tcp://<its 100.x address>:22000, quic://<its 100.x address>:22000`, or keep `dynamic` to rely on Syncthing's discovery.
3. Accept the request that appears in the other computer's web UI, then share folders from the **Edit** dialog of each folder.

SyncThing V2 shows devices and folders paired this way like any others.

## Non-default ports

SyncThing V2's discovery knocks on port **22000** only. If Syncthing on a computer listens on another port (you changed **Sync Protocol Listen Addresses**, or 22000 was taken when Syncthing was first set up), then:

- `stv2 doctor` on that computer reports [ST005](troubleshooting.md#st005)
- other computers show it as "Install SyncThing V2 on this device (or it uses a non-default port)"

You can still pair from that computer's side, because it can reach the others on 22000. The others then learn its real port when it connects. Or you can pair by hand as described above, with the real port in the address. To go back to the default, set the listen address to `default` in Syncthing's web UI (**Settings → Connections**) and restart Syncthing.
