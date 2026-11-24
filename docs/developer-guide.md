# Developer guide: building a client for the CPDLC FIR proxy

This guide is for developers of ATC clients (EuroScope plugins, standalone
programs) that want to use the proxy. It describes the wire protocol the proxy
expects, what it does on its own, and what a client has to do so that several
sectors can share one Hoppie station.

The behaviour described here is what the code in this repository does; the
section numbers are referenced from comments in the source.

Contents:

1. [What the proxy does](#1-what-the-proxy-does)
2. [Hoppie in short](#2-hoppie-in-short)
3. [Connecting to the proxy](#3-connecting-to-the-proxy)
4. [Reporting flights: `assumed`](#4-reporting-flights-assumed)
5. [Polling and proxy notices](#5-polling-and-proxy-notices)
6. [Sending uplinks](#6-sending-uplinks)
7. [How downlinks are routed](#7-how-downlinks-are-routed)
8. [Transfers inside the FIR](#8-transfers-inside-the-fir)
9. [Leaving the FIR](#9-leaving-the-fir)
10. [Outages](#10-outages)
11. [PDC](#11-pdc)
12. [Security model](#12-security-model)
13. [Client checklist](#13-client-checklist)
14. [Why existing ATC clients don't work as they are](#14-why-existing-atc-clients-dont-work-as-they-are)
15. [Testing a client](#15-testing-a-client)

---

## 1. What the proxy does

Without the proxy, every sector logs on to Hoppie with its own station callsign
(`MWS2`, LIMM_WS2_CTR). A pilot must log on again at every sector
change, and two sectors can never share a station: Hoppie hands each message to
whoever polls first, and locks a polled callsign to one logon code for 100 s.

With the proxy there is one Hoppie station per FIR (`LIRR`). Only the proxy
polls it and sends as it. Sectors talk to the proxy instead of Hoppie:

```
LIRR_N_CTR ─┐  Hoppie-style form, from=LIRR_N_CTR, logon=<controller's own code>
LIRR_S_CTR ─┼──► proxy ──(only poller, from=LIRR)──► Hoppie
LIMM_W_CTR ─┘      └────(only poller, from=LIMM)──► Hoppie
```

The proxy:

- authorizes each sector by its callsign and Hoppie logon code (§3);
- learns which sector owns which aircraft from what sectors report (§4);
- routes every downlink to the right sector (§7);
- rewrites message numbers so sectors never collide (§6);
- keeps the CPDLC connection alive across internal sector changes, and answers
  the pilot itself when no CPDLC sector owns the flight (§8);
- handles outages and stale messages (§10);
- arbitrates pre-departure clearances between the sectors of an airport (§11).

A FIR is either proxied or not: if any other client polls `LIRR` directly while
the proxy does, the two steal each other's messages and Hoppie answers one of
them `callsign already in use`.

## 2. Hoppie in short

The proxy speaks a subset of Hoppie's `connect.html` API, so a client that
already talks to Hoppie keeps most of its code.

### Transport

- Requests are HTTP form posts (GET also works) with the fields `logon`,
  `from`, `to`, `type` and `packet`.
- Types: `poll` (fetch pending messages, `to=SERVER`), `cpdlc` (send one CPDLC
  message), `telex` (send free text), `ping` (connectivity check).
- A poll reply is `ok {CS1 cpdlc {/data2/...}} {CS2 telex {text}}`. An error
  reply is `error {reason}`.
- Hoppie only queues messages. `ok` on a send never means the aircraft got it.
- Hoppie keeps queued messages for hours and gives them no timestamp.

### Packet format

```
/data2/<min>/<mrn>/<ra>/<TEXT>
```

- `min`: the sender's message number. Always send one; some pilot clients fail
  on an empty MIN.
- `mrn`: the MIN of the message this one answers; empty for a new message.
- `ra`: the reply the sender expects. Uplinks: `WU` (WILCO/UNABLE), `AN`
  (AFFIRMATIVE/NEGATIVE), `R` (ROGER), `NE` (none). Downlinks: `Y` or `N`.
- Variables sit between `@` signs; `@_@` is a line break:
  `CONTACT @LIMM 135.455@_@MILANO CTR`.

Split a packet on its first five `/` only: the text may contain `/`.

### Things seen live that a parser must accept

| Seen | Handling |
|---|---|
| Closing `@` missing | The last variable runs to the end of the text |
| Empty MIN: `/data2///N/LOGOFF` | Accept it |
| WILCO with RA `N` or `NE` | Never validate replies by their RA |
| The same reply twice, or a repeated `REQUEST LOGON` | Handle every transition idempotently |
| Requests without `@`: `REQUEST DIRECT TO FIX ARTIP` | Classify on the plain text |
| Requests with RA `N` | Still answer them |
| A logon queued many hours ago | Check the flight is online before accepting it |

### Rules for uplinks

1. Don't put the aircraft's callsign in a CPDLC text: common pilot clients drop
   everything before it. (PDC texts are the exception, §11.)
2. Only `CURRENT ATC UNIT` should contain a `_@WORD@_` pattern: some pilot
   clients take any such pattern as the new current unit.
3. No `{` or `}`: they break Hoppie's framing. Write `/` inside a value as `|`.
4. Mark variables with `@...@`, and send clearances with RA `WU`.

## 3. Connecting to the proxy

### Requests

Every request is a Hoppie form post to the proxy's URL (any path), with:

| Field | Value |
|---|---|
| `logon` | The controller's own Hoppie logon code |
| `from` | The controller's own callsign (`LIRR_N_CTR`), **not** the station |
| `to` | Aircraft callsign, or `SERVER` for a poll |
| `type` | `poll`, `cpdlc`, `telex`, `ping` or `assumed` |
| `packet` | As for Hoppie |
| `assumed` | Optional, on a poll: the sector's flights (§4) |
| `pdc` | `1` on an uplink that answers a PDC request (§11) |

Bodies are limited to 64 KiB. Use HTTPS: the logon code is in every request.

### Authorization

On every request the proxy checks, in order:

1. `from` matches a sector pattern of one of its stations
   (`LIRR_*_CTR` → `LIRR`). Otherwise: `error {callsign not authorized on this proxy}`.
2. `logon` is not empty, and a Hoppie `ping` with that code and callsign
   answers `ok`. The result is cached per (code, callsign): 5 min when valid,
   1 min when refused. Refused: `error {illegal logon code}`.
3. No other logon code has used that callsign in the last 30 s. Otherwise:
   `error {callsign already in use}`.

At most 5 uncached checks go to Hoppie per 10 s. Past that the proxy answers
`error {proxy busy, retry}`; retry on the next poll.

A client should treat these as follows:

| Reply | Meaning | Client action |
|---|---|---|
| `error {callsign not authorized on this proxy}` | Not a sector of a proxied FIR | Stop, tell the controller |
| `error {illegal logon code}` | Bad Hoppie code | Stop, tell the controller |
| `error {callsign already in use}` | Another code uses this callsign | Stop, tell the controller |
| `error {proxy busy, retry}` | Rate limit on logon checks | Retry on the next poll |
| `error {hoppie unreachable}` | The proxy cannot reach Hoppie (§10) | Keep polling, alert after a few in a row |
| `error {unknown type …}`, `error {no recipient}` | Client bug | Log it |

A sector counts as **connected** while it has made any request in the last
30 s. After 5 min without requests it is forgotten, with any messages still
waiting for it.

### Which station a sector belongs to

A callsign uses the first station whose pattern matches it, in the order of the
config file. The client never names the station; it is derived from `from`.

## 4. Reporting flights: `assumed`

The proxy has no radar picture. It learns who owns what from each sector, in a
three-part field:

```
<assumed>|<next>|<released>
```

- `<assumed>`: aircraft this sector has assumed (is tracking).
- `<next>`: aircraft for which this sector is the coordinated next sector or
  the handoff target. This lets a pilot log on before entering the FIR.
- `<released>`: optional, `CS=TRACKER` pairs. For flights the proxy knows as
  connected but no sector serves (they were released inside the FIR, or the
  proxy parked them, §8), the controller currently tracking them according to
  the client, or `CS=` when nobody tracks them. The proxy uses it to tell a
  flight still inside the FIR from one that has left (§9).

Callsigns are separated by spaces. Example:
`AZA1 RYR2|BAW3|DLH4=LIRR_NE7_CTR`. Empty parts are fine: `||`.

Send it in the `assumed` field of **every poll**. It is also a heartbeat: the
whole set replaces what the sector reported before, so a flight that is missing
from it is released. `type=assumed` with the same text in `packet` does the
same without polling, for an immediate update after a change.

If two sectors claim one aircraft, the latest report wins (and the proxy logs it).

Sectors that only answer PDC requests (delivery, tower) send an empty
`assumed`, or none at all.

## 5. Polling and proxy notices

Poll the proxy every **3–5 s** with `type=poll`, `to=SERVER` and `assumed`.
The proxy itself polls Hoppie every 40 s, and every 20 s for 3 min after any
sector sent an uplink that expects a reply. It doesn't poll a station with no
connected sector.

A sector that hasn't polled for 30 s is considered gone (§3), so polling slower
than that breaks routing and parks the sector's flights.

The reply is Hoppie's format. Besides `cpdlc` and `telex` messages from
aircraft, it can contain proxy notices with the type `vcp`:

| Notice | Meaning | Client action |
|---|---|---|
| `{AZA1 vcp {CONNECTED AUTH}}` | AZA1 has a CPDLC connection to the station and is authorized; you now own it | Create the connection in your state, as current data authority |
| `{AZA1 vcp {CONNECTED NOTAUTH}}` | AZA1 is connected but voice only (it was parked, §8) | Create the connection; send `CPDLC NOW IN USE` if you want to use CPDLC with it |
| `{AZA1 vcp {PDC SENT BY LIRF_DEL}}` | Another sector answered AZA1's PDC request | Close the request in your list (§11) |

A `CONNECTED` notice always comes before any other message for that aircraft
in the same reply. The client adopts the flight as it is: it did not see the
logon, and must not ask the pilot to log on again.

## 6. Sending uplinks

Send `type=cpdlc` or `type=telex` with `to` = aircraft. The proxy sends it to
Hoppie **as the station** (`from=LIRR`, with the proxy's own logon code), at
least 2 s after its previous send as that station, and returns Hoppie's reply.
A send can take a few seconds when several sectors send at once.

### MIN rewrite

Each sector keeps its own MIN counter, as with plain Hoppie. The proxy replaces
the MIN of every CPDLC uplink with one from a counter per (station, aircraft),
remembers which sector used which local MIN, and maps the `mrn` of the pilot's
reply back to the sector's local MIN. The client sees its own numbers only.
The mapping is kept for an hour.

### Connection tracking

The proxy infers each aircraft's connection from the uplinks it forwards:

| Uplink text | Effect |
|---|---|
| `LOGON ACCEPTED…` | Connected and authorized; this sector knows the connection |
| `CPDLC NOT IN USE…` | Not authorized (voice only) |
| `CPDLC NOW IN USE…` | Authorized again |
| `HANDOVER…` | Going to another station: the proxy will not park it (§9) |
| `LOGOFF` | Connection closed |

A pilot's `REQUEST LOGON` or `LOGOFF` also ends the previous connection.

Use these texts verbatim so the proxy can follow the connection.

## 7. How downlinks are routed

Every message the proxy polls from Hoppie goes to the first that applies:

1. **The reply rule:** a message whose `mrn` matches an uplink goes to the
   sector that sent that uplink, if it is connected, with the `mrn` rewritten
   to that sector's MIN. Replies reach whoever opened the dialogue, even after
   the flight moved to another sector.
2. **The owner:** the sector that has the aircraft in `<assumed>`. If that
   sector doesn't know the connection yet, it first gets `CONNECTED AUTH` or
   `CONNECTED NOTAUTH` (§5).
3. **The next sector:** the sector that has the aircraft in `<next>`, if no
   connection exists yet or that sector already knows it. This is how a pilot
   logs on before entering the FIR.
4. **Nobody:** the proxy answers by itself:
   - `REQUEST LOGON`: accepted and parked (§8).
   - A request (RA `Y`, `REQUEST…`, `WHEN CAN…`) from a parked flight:
     `UNABLE@_@CPDLC NOT IN USE. VOICE ONLY` on its MIN.
   - Anything else is logged and dropped.

PDC requests (telex containing `PREDEP`) follow their own rules (§11).

Logons routed to a sector are that sector's to accept or reject: the proxy does
not see flight plans. A client should accept only a flight that is online with
a flight plan, and send `LOGOFF` otherwise.

## 8. Transfers inside the FIR

The station stays `LIRR` for the whole FIR crossing, so the connection survives
every internal transfer. The client does **not** hand over between sectors of
the same FIR:

- **Transfer to another sector of the FIR:** send `CONTACT` (frequency change)
  only. No `HANDOVER`, no `LOGOFF`. When the pilot WILCOs (or the transfer
  completes), stop reporting the flight in `<assumed>`. Close your open
  dialogues locally; replies to them still reach you through rule 1 of §7.
- **The new sector** gets `CONNECTED AUTH` as soon as it reports the flight in
  `<assumed>`, and takes over without a new logon.
- **The new sector doesn't use CPDLC** (a client without proxy support, CPDLC
  off, or no controller): after 30 s with no connected sector owning the flight,
  the proxy sends `CPDLC NOT IN USE. VOICE ONLY UNTIL NOTIFIED` and **parks**
  it. While parked:
  - pilot requests get the automatic UNABLE (§7);
  - a pilot `LOGOFF` closes the connection;
  - a new logon is accepted by the proxy with `LOGON ACCEPTED`,
    `CURRENT ATC UNIT@_@<station>@_@<facility>` and the NOT IN USE message.
- **A parked flight is assumed by a CPDLC sector:** that sector gets
  `CONNECTED NOTAUTH`. It should check the flight plan (and send `LOGOFF` if
  there is none), then send `CPDLC NOW IN USE` to authorize it.
- **A sector stops polling with assumed flights:** 30 s after it is considered
  gone (so about a minute after its last poll), its flights are parked.

The 30 s grace covers the gap between the old sector's last report and the new
one's first. Nobody needs to know in advance which sectors use CPDLC: the pilot
is only told when that changes.

If a sector drops a flight from `<assumed>` and later reports it again, it is
sent `CONNECTED` again: on dropping a flight, forget its connection locally.

## 9. Leaving the FIR

- **From a CPDLC sector to another unit:** the normal Hoppie handover: send
  `CONTACT`, then `HANDOVER @<next station>` (`HANDOVER @LIMM`). For a neighbouring FIR on the same
  proxy, the next station is that FIR's station (`LIMM`). Once a `HANDOVER` went
  through the proxy, it waits for the pilot's `LOGOFF` and never parks the
  flight (it forgets it after 24 h if no LOGOFF comes).
- **From a sector without CPDLC:** the proxy learns it from `<released>`: when
  any sector reports a parked or unserved flight tracked by a controller whose
  callsign matches none of this FIR's patterns, the proxy sends
  `ATC SERVICE TERMINATED` and `LOGOFF` at once.
- **No news:** a flight parked for 30 min without a report of a tracker inside
  the FIR gets the same logoff.

## 10. Outages

### Proxy side

- After 3 failed Hoppie polls in a row, sector polls answer
  `error {hoppie unreachable}`. Messages already in sector inboxes wait.
- With `-state`, the proxy saves connections, parked flights, MIN mappings and
  the time of its last Hoppie poll, atomically at most once a second and on
  shutdown. Owners are not saved: sectors resend them with every poll. After a
  restart, each sector gets `CONNECTED …` again for its assumed flights.
- **Stale batches:** Hoppie gives no timestamps, so the first poll after a gap
  longer than `stale_seconds` (180 s) is treated as an outage batch: pilot
  requests in it are answered `CPDLC TEMPORARILY UNAVAILABLE. REVERT TO VOICE`
  and not delivered. Replies, logons and logoffs go through.
- After a gap longer than 10 min (including a station's first poll after the
  proxy starts), a logon that no sector owns or expects, and any PDC request,
  is dropped without an answer: the pilot has most likely left, and one still
  there asks again.

### Client side

- Don't fall back to polling Hoppie directly: that clashes with the proxy when
  it comes back, and makes every pilot log on again.
- Alert the controller after about 3 failed polls in a row, not on one failure,
  and tell the causes apart: proxy unreachable (HTTP error),
  `error {hoppie unreachable}`, or an authorization error (§3).
- Let open dialogues time out as usual.

## 11. PDC

Pilots ask for a pre-departure clearance by telex:

```
REQUEST PREDEP CLEARANCE <CS> <TYPE> TO <DEST> [AT <DEP>] [STAND <n>] [ATIS <x>]
```

Any telex containing `PREDEP` is a PDC request. Stations with `"pdc": true` are
airports (`LIRF`). Their sectors (delivery, ground, tower, approach) connect to
the proxy with their own callsigns, matched by the airport's patterns, and
usually send no flights.

### What the proxy does with a request

- **Well formed, for this airport** (`AT` is the station, or no `AT`): delivered
  to **every** connected sector of the airport. If none is connected, it goes to
  every connected sector of the airport's `top_down` CTR station instead.
- **Repeat before anyone answered:** answered
  `<CS> RCD REJECTED REQUEST ALREADY RECEIVED STANDBY`, not delivered again.
  Once a sector has answered, a new request opens the race again.
- **Sent to the wrong station:** answered `PDC NOT AVAIL. RELAY TO @<airport>@`
  when the proxy knows the right airport (the `AT` airport, or the single
  airport a CTR station covers top-down), else
  `<CS> RCD REJECTED REVERT TO VOICE PROCEDURES`.
- **Malformed:** `<CS> RCD REJECTED ERROR IN MESSAGE REVERT TO VOICE PROCEDURES`.
- **Stale** (§10): `<CS> RCD REJECTED REVERT TO VOICE PROCEDURES`, or dropped
  after a gap longer than 10 min.

All proxy answers are CPDLC with RA `NE`. A request is forgotten after 30 min,
and pending requests are not saved across a proxy restart.

### What the client does

1. **Acknowledge** a new request when the flight plan is held: send the CPDLC
   text `<CS> RCD RECEIVED REQUEST BEING PROCESSED STANDBY` with `pdc=1`. Every
   sector may send it; the proxy sends it to the pilot once and drops it once
   the request is answered. If the flight plan is not held, send
   `<CS> RCD REJECTED FLIGHT PLAN NOT HELD REVERT TO VOICE PROCEDURES`
   (RA `NE`, `pdc=1`) instead.
2. **Answer** with CPDLC and `pdc=1`:
   - the clearance with RA `R`, starting with the callsign:
     `AZA123 CLRD TO @LIMC@ OFF @16R@ VIA @SID1A@ SQUAWK @4521@ …`. The proxy
     puts `CLD <hhmm>Z <yymmdd> <airport> PDC <MIN>` in front of it.
   - or a rejection: `<CS> RCD REJECTED REVERT TO VOICE PROCEDURES`, RA `NE`.
   A telex answer also works and takes part in the race, without the header.
3. **The race:** the first sector to answer takes the request. The others
   get `{<CS> vcp {PDC SENT BY <sector>}}`; if they still send a `pdc=1` uplink
   or a telex to that aircraft, it is refused with
   `error {pdc already sent by <sector>}` and never reaches the pilot. The
   sector that took it may send again.
4. **Confirmation:** when the pilot answers ROGER (or ACCEPT) to the clearance,
   the proxy sends `<CS> CDA RECEIVED CLEARANCE CONFIRMED` once and passes the
   ROGER to the sector that sent the clearance.

A `pdc=1` uplink with no request pending (it expired, or the proxy restarted)
is refused with `error {no pdc request pending}`: drop the request locally.

PDC uplinks go out as the airport station the pilot asked, even when a CTR
sector answers top-down. Uplinks without `pdc=1` never touch a PDC: a CTR's
`LOGON ACCEPTED` to an aircraft with a PDC pending stays its own.

No CPDLC logon is needed for a PDC.

## 12. Security model

- The proxy holds one Hoppie logon code of its own and uses it for everything
  sent as a station. Controllers keep their own codes; the proxy only checks
  them with a Hoppie `ping`.
- Anyone with a valid Hoppie code can act as a sector by using a callsign that
  matches a pattern. The proxy cannot see the VATSIM network; the client should
  enable CPDLC only on a live network connection, under the callsign the
  controller is connected with.
- Logon codes travel in every request: serve the proxy over HTTPS only, and
  refuse plain `http://` proxy URLs other than localhost in the client.

## 13. Client checklist

Transport:

- [ ] A setting for the proxy URL; HTTPS only (localhost allowed for testing).
- [ ] `from` = the controller callsign, `logon` = the controller's own code.
- [ ] Poll every 3–5 s, never slower than 30 s.
- [ ] Handle every `error {…}` reply of §3, §10 and §11.

Ownership:

- [ ] Send `<assumed>|<next>|<released>` with every poll, and with
      `type=assumed` on every change.
- [ ] Report the coordinated next sector / handoff target in `<next>`.
- [ ] Report trackers of known connections you don't own in `<released>`.

Messages:

- [ ] Handle the `vcp` notices of §5; adopt connections without a new logon.
- [ ] Forget a connection locally when you stop reporting the flight.
- [ ] Use the exact texts of §6 for logon acceptance, NOT/NOW IN USE,
      HANDOVER and LOGOFF.
- [ ] Inside the FIR: `CONTACT` only, then release. Between FIRs: `CONTACT`,
      `HANDOVER`.
- [ ] Accept a logon only for an online flight with a flight plan.

PDC (if the client serves airports):

- [ ] Show requests to every sector of the airport; close them on
      `PDC SENT BY`.
- [ ] Mark the acknowledgement, clearance and rejection with `pdc=1`.
- [ ] Start the clearance text with the callsign; don't add a `CLD` header.

## 14. Why existing ATC clients don't work as they are

Several ATC clients let you change the Hoppie URL, so it looks as if they could
point at the proxy unchanged. They can't: the proxy is Hoppie-compatible on the
wire, but sharing a station needs information and behaviour that a plain Hoppie
client neither sends nor expects.

1. **They don't say which flights they own.** Without `assumed`, the proxy has
   no owner or next sector for any aircraft. Only replies to that client's own
   uplinks reach it (rule 1 of §7). Every new logon is accepted by the proxy
   itself and parked as voice only, and every pilot request gets the automatic
   UNABLE. The controller never sees them.

2. **They poll too slowly.** Hoppie asks clients to poll every 45–75 s. The
   proxy considers a sector gone after 30 s of silence, so such a client is
   offline most of the time: messages for it are routed elsewhere or answered
   by the proxy, and 30 s later its flights are parked.

3. **They don't understand the proxy's notices.** A flight transferred to them
   inside the FIR arrives with `{CS vcp {CONNECTED AUTH}}` instead of a logon.
   A plain client ignores an unknown message type, so it believes the aircraft
   is not connected: it can't send it anything, and treats its messages as
   coming from an unconnected aircraft.

4. **They hand over between sectors.** A plain client transfers with
   `HANDOVER` and expects the pilot to log off and log on to the next unit.
   Inside a proxied FIR the next unit is the same station, so the handover
   breaks the connection the proxy is trying to keep; and once the proxy sees a
   `HANDOVER` it stops parking the flight and waits for a logoff.

5. **PDC needs the `pdc=1` marker.** Without it a CPDLC clearance is an
   ordinary uplink: no `CLD` header, no race between sectors, no
   `CDA RECEIVED CLEARANCE CONFIRMED`, and from a top-down CTR it goes out as
   the CTR station instead of the airport the pilot asked. Telex answers do
   take part in the race, but other sectors' `PDC SENT BY` notices are still
   ignored, so several controllers may prepare the same clearance.

6. **New error replies.** Replies such as `error {proxy busy, retry}`,
   `error {hoppie unreachable}` or `error {pdc already sent by …}` are normal
   operating conditions on the proxy. A plain client may show them as fatal
   errors or disconnect.

In short, the URL setting makes the transport compatible, but the coordination
between sectors lives in the extra fields and messages. Supporting the proxy
means implementing §3–§9 (and §11 for airports), which is mostly new logic
around an existing Hoppie client rather than changes to it.

There is also an operational consequence: a FIR cannot be half proxied. If
one controller in `LIRR` uses the proxy and another polls `LIRR` directly with
an unmodified client, both lose messages to each other.

## 15. Testing a client

- `go test ./...` in this repository covers the proxy's own rules.
- Run the proxy locally against a Hoppie simulator:
  `"hoppie_url": "http://127.0.0.1:<port>/acars/system/connect.html"` and
  `"poll_seconds": 2` in the config, then point the client at
  `http://127.0.0.1:8080/`.
- Worth scripting:
  - transfer between two CPDLC sectors (`CONNECTED AUTH` on the second);
  - transfer through a sector without CPDLC (NOT IN USE, automatic UNABLE,
    NOW IN USE on the third);
  - logon before FIR entry through `<next>`;
  - a reply arriving after an internal transfer (goes to the old sector);
  - two sectors claiming one aircraft;
  - a sector that stops polling with assumed flights;
  - proxy restart, Hoppie down, stale requests after recovery;
  - two airport sectors answering the same PDC.
