# Changelog

## [0.2.1](https://github.com/yaad-index/bonyan/compare/memory/honcho/v0.2.0...memory/honcho/v0.2.1) (2026-10-05)


### Bug Fixes

* **memory/honcho:** bring an existing workspace's deriver setting to the Backend's ([#75](https://github.com/yaad-index/bonyan/issues/75)) ([64efcaf](https://github.com/yaad-index/bonyan/commit/64efcaf96cb7a961744323c2cbf24df16d498c1a))
* **memory/honcho:** wait only for the subject's own workspace when erasing ([#73](https://github.com/yaad-index/bonyan/issues/73)) ([3b9b810](https://github.com/yaad-index/bonyan/commit/3b9b810bff4b01942fb01f0b6b68720adbbd24a3))

## [0.2.0](https://github.com/yaad-index/bonyan/compare/memory/honcho/v0.1.0...memory/honcho/v0.2.0) (2026-10-05)


### ⚠ BREAKING CHANGES

* **memory/honcho:** memory kept with the per-subject layout is not read. Delete it with the earlier version before upgrading (ADR 0003 §6). DeleteWait now bounds the wait for the deriver's queue during an erase.

### Features

* **memory/honcho:** keep one workspace per namespace ([#71](https://github.com/yaad-index/bonyan/issues/71)) ([cd83dbc](https://github.com/yaad-index/bonyan/commit/cd83dbc0a5fd5e8b37a9909a6d34c50dc143b168))

## 0.1.0 (2026-10-04)


### Features

* **memory/honcho:** add a backend over the external memory service ([#65](https://github.com/yaad-index/bonyan/issues/65)) ([692bcd3](https://github.com/yaad-index/bonyan/commit/692bcd33347aa55ff0f5f0b66253d07d90e844c1))
