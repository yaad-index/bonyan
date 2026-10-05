# Changelog

## [0.2.0](https://github.com/yaad-index/bonyan/compare/memory/honcho/v0.1.0...memory/honcho/v0.2.0) (2026-10-05)


### ⚠ BREAKING CHANGES

* **memory/honcho:** memory kept with the per-subject layout is not read. Delete it with the earlier version before upgrading (ADR 0003 §6). DeleteWait now bounds the wait for the deriver's queue during an erase.

### Features

* **memory/honcho:** keep one workspace per namespace ([#71](https://github.com/yaad-index/bonyan/issues/71)) ([cd83dbc](https://github.com/yaad-index/bonyan/commit/cd83dbc0a5fd5e8b37a9909a6d34c50dc143b168))

## 0.1.0 (2026-10-04)


### Features

* **memory/honcho:** add a backend over the external memory service ([#65](https://github.com/yaad-index/bonyan/issues/65)) ([692bcd3](https://github.com/yaad-index/bonyan/commit/692bcd33347aa55ff0f5f0b66253d07d90e844c1))
