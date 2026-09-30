# Changelog

## [1.2.0](https://github.com/alex-bluetrain/notuya-gui/compare/v1.1.0...v1.2.0) (2026-09-30)


### Features

* **gui:** unify the Settings tab with the first-run wizard ([6eaf2a0](https://github.com/alex-bluetrain/notuya-gui/commit/6eaf2a00274674fb275a087e64ce57716c58f113))

## [1.1.0](https://github.com/alex-bluetrain/notuya-gui/compare/v1.0.0...v1.1.0) (2026-09-30)


### Features

* **gui:** add first-run setup wizard for empty configs ([7ed9cf0](https://github.com/alex-bluetrain/notuya-gui/commit/7ed9cf0672a4c16bf014178b5077614d77b64d02))


### Bug Fixes

* **gui:** give each light one control, and Power a label of its own ([5333c71](https://github.com/alex-bluetrain/notuya-gui/commit/5333c711a3d4a9001924285fb613be54f6252c8d))
* **gui:** make the wizard test a fast flash that restores the bulb ([80f73a4](https://github.com/alex-bluetrain/notuya-gui/commit/80f73a4981dade26bede0fffe91a21f8aa1c7e6e))
* **gui:** make the wizard test flash a fast strobe (~200ms) ([03b484d](https://github.com/alex-bluetrain/notuya-gui/commit/03b484d40e9cd8c170fc6ac276661ff542471bc7))
* **gui:** stop mangling local keys and widen the wizard key field ([dc12371](https://github.com/alex-bluetrain/notuya-gui/commit/dc12371cb7f4daec6e1f12f416512f32e7bee239))
* **gui:** wizard test blinks white three times in ~200ms ([dbccbe7](https://github.com/alex-bluetrain/notuya-gui/commit/dbccbe7827c60b3618d1f35b23a4d5d42b5bc424))
* **gui:** wizard test blinks white with discrete writes, leaves bulb on ([6398d56](https://github.com/alex-bluetrain/notuya-gui/commit/6398d562d628d8bd5545e1d7f31bc3fa70092152))

## 1.0.0 (2026-09-28)


### Features

* add settings window for editing devices and LAN discovery ([3f57788](https://github.com/alex-bluetrain/notuya-gui/commit/3f577882a6723ec21073f4ee65de110b2b7a7759))
* dual-mode desktop app with rooms, scenes, and Adwaita UI ([86ab761](https://github.com/alex-bluetrain/notuya-gui/commit/86ab761dbe11eed0214fda28c0c1bb4a48db6588))
* English UI, dedicated Rooms tab, and scene editor ([63c4267](https://github.com/alex-bluetrain/notuya-gui/commit/63c42672f1e211c39a6a4183e665a77277a6763d))
* **gui:** Hue-level polish for the Lights tab ([cfdb4d1](https://github.com/alex-bluetrain/notuya-gui/commit/cfdb4d154348d93937a25d63523768245c079381))
* **gui:** live per-light state rows in Lights tab (Pass B) ([abbdb01](https://github.com/alex-bluetrain/notuya-gui/commit/abbdb0101e1045b066c4e63f3ac94d59f8ba9fe6))
* **gui:** native temperature slider and consistent Lights layout ([1358e6c](https://github.com/alex-bluetrain/notuya-gui/commit/1358e6c1ffc4080cbaafe0a1994bb33a481cf833))
* **gui:** per-light rows mirror shared-control broadcasts ([4e2d7ac](https://github.com/alex-bluetrain/notuya-gui/commit/4e2d7ac4ea66b8316d638f36d6b18da74cd395e0))
* **gui:** polish Lights tab controls (Pass A) ([f15e446](https://github.com/alex-bluetrain/notuya-gui/commit/f15e4463358868c1a6a408c8fd398f7d9e08d5d9))
* **gui:** redesign Lights and Scenes tabs, add fade control ([6557cd8](https://github.com/alex-bluetrain/notuya-gui/commit/6557cd8334ca60dcd238172a9faef0250b5a6879))
* redesign Lights tab as a shared colour-wheel playground ([8db094e](https://github.com/alex-bluetrain/notuya-gui/commit/8db094efbd87dfb91d4ddef2957a407712a23a30))
* remove non-functional firmware scene buttons ([b4811ad](https://github.com/alex-bluetrain/notuya-gui/commit/b4811ade5c1c72df71016a578795e206f504a4be))
* **test:** working full-height headless capture via cage subcommand ([60d337a](https://github.com/alex-bluetrain/notuya-gui/commit/60d337aef7ed7d5fc141ee38796dbaade32627a3))


### Bug Fixes

* **gui:** exit cleanly on window close; complete the AT-SPI test loop ([c60c649](https://github.com/alex-bluetrain/notuya-gui/commit/c60c64993084122cc99b9230805d02385ab317f6))
* **gui:** per-light rows read colour-mode brightness from DP 24 ([7985df6](https://github.com/alex-bluetrain/notuya-gui/commit/7985df6d6f1c628c33b3db04784425bbf2f0fa72))
* **gui:** wheel thumb previews hue/sat only, not brightness ([007ca8b](https://github.com/alex-bluetrain/notuya-gui/commit/007ca8bf6832d79c7c266c5f6720919630fe2b60))
* **gui:** wheel thumb reaches the disc edge at full saturation ([7c91212](https://github.com/alex-bluetrain/notuya-gui/commit/7c9121252e8c226f7d8f031f7be0e9ae920922c5))
