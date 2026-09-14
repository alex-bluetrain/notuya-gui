# The tinytuya Setup Wizard — how it obtains `local_key`

This document explains what the tinytuya setup wizard actually does, why it
needs the Tuya cloud, and where that leaves a cloud-free tool like
`notuya-gui`.

> The wizard is **not** part of `notuya-gui` and never will be — this repo
> deliberately does not touch Tuya's cloud (see `AGENTS.md`). This is
> reference material for users who need to recover their keys with an external
> tool before pasting them into the settings window by hand.

## Why a wizard is needed at all

The `local_key` is a 16-character secret that a Tuya bulb uses to
[AES-encrypt every LAN payload](https://deepwiki.com/jasonacox/tinytuya/2.2-initial-setup-and-configuration).
<cite index="8-5,8-6">TinyTuya requires specific cryptographic keys (Local Keys) to communicate with devices over the LAN; these keys are not broadcast by the devices and must be retrieved from the Tuya IoT Platform.</cite>
A LAN scan can find a bulb's IP, device ID, and protocol version — but never
the key. The key only ever leaves Tuya's cloud, which is exactly why a cloud
round-trip is unavoidable.

## Prerequisites (one-time, all manual)

These happen **before** you run the wizard and are the real cost of the whole
approach:

1. **Pair every device in the phone app.**
   <cite index="2-3,2-4">Download the Smart Life or Tuya Smart App and pair all of your Tuya devices — this is important as you cannot access a device that has not been paired.</cite>
   <cite index="4-6">Do not use a 'guest' account, otherwise it will get deleted without confirmation.</cite>

2. **Create a Tuya IoT developer account and a Cloud project** at
   `iot.tuya.com`, picking the data-center region for your location.

3. **Link your app account to the project.**
   <cite index="1-17,1-18">Under Cloud → your project → Devices → Link Tuya App Account, click Add App Account, choose "Automatic" and "Read Only Status" (it still allows commands), click OK and it displays a QR code</cite>
   which you scan with the phone app. This is what lets the project *see* the
   devices you paired in step 1.

4. **Note your credentials.** On the project's Cloud → Development page,
   <cite index="6-15">apiKey = Access ID and apiSecret = Access Secret.</cite>
   You also need the region code
   (<cite index="4-7">cn, us, us-e, eu, eu-w, sg, or in</cite>).

## The wizard run itself

Invoke it with:

```bash
python -m tinytuya wizard      # add -nocolor for non-ANSI terminals
```

<cite index="4-7">The wizard prompts you for the API ID key, API Secret, and API Region from your Tuya IoT project.</cite>

Step by step, the wizard then:

1. **Optionally scans the LAN.**
   <cite index="8-11,8-12">The discovery process uses tinytuya.scanner.devices() to locate hardware on the LAN, listening for UDP broadcasts on ports 6666, 6667, and 7000.</cite>
   This yields IP + device ID + version per device — no key.

2. **Authenticates to the cloud** by signing a request with your API ID and
   Secret to obtain an access token.

3. **Fetches the device registry** for the linked app account.
   <cite index="10-1,10-2">It polls the Tuya IoT Cloud Platform and prints a JSON list of all registered devices with the "name", "id" and "key" — the "key" values are the devices' Local_Key you use to access them.</cite>

4. **Merges cloud + LAN and writes files.** The registry (which has the keys)
   is joined with the LAN scan (which has the IPs) by matching device IDs, and
   the result is persisted:
   <cite index="8-3">the wizard retrieves the local key from the Cloud API and stores it in devices.json.</cite>
   It also writes snapshot/raw JSON files alongside it.

## Sequence diagram

See [`wizard.puml`](./wizard.puml). Render it with any PlantUML tool, e.g.:

```bash
plantuml docs/wizard.puml          # -> docs/wizard.png
# or, no local install:
#   paste the file into https://www.plantuml.com/plantuml
```

## After the wizard

From this point communication is LAN-only: AES with the `local_key` over TCP
6668, no cloud required until keys are re-issued (e.g. after re-pairing or a
factory reset). You copy each `id` / `ip` / `key` into your own tool.

For **notuya-gui** specifically:

1. Run the wizard once (above) to produce `devices.json`.
2. Open the settings window: `notuya-gui -config`.
3. For each bulb, paste the `id`, `ip`, and `key` into the form and Save.

notuya-gui's own LAN discovery covers the IP + device ID half automatically;
the wizard is only ever needed to recover the `local_key` half.

## Common failure modes

- **"your ip … don't have access"** — usually a project-side configuration
  problem.
  <cite index="6-11">Ensure you have subscribed and authorized these API services: IoT Core, Authorization and Smart Home Scene Linkage.</cite>
  <cite index="6-13">Also disable popup blockers, otherwise subscribing won't work without any indication of failure.</cite>
- **Scan finds nothing** — this can indicate an expired IoT Core subscription;
  the wizard will let you enter a device ID manually.
- **Wrong key length / connection error** —
  <cite index="8-16,8-17,8-18">the key must be exactly 16 characters; incorrect lengths (often from shell escaping of characters like $, #, or !) cause connection failures (Error 914).</cite>

## Alternatives to the wizard

- **TuyAPI CLI** — the tool the wizard was inspired by.
  <cite index="10-13">Install with `npm i @tuyapi/cli -g` and run `tuya-cli wizard`.</cite>
  Same cloud dependency, different implementation.
- **App-data extraction** (rooted Android / iOS backup / mitmproxy capture) —
  pulls the key without a developer account, but is fiddly and per-device.
- **Flash open firmware** (Tasmota / ESPHome) — the only path that eliminates
  `local_key` entirely, at the cost of leaving the Tuya protocol behind.
