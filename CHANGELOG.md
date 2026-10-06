# Changelog

## [0.2.1](https://github.com/yaad-index/bonyan/compare/v0.2.0...v0.2.1) (2026-10-06)


### Bug Fixes

* **telemetry:** histogram buckets sized for seconds, tokens and scores ([#79](https://github.com/yaad-index/bonyan/issues/79)) ([5cc035e](https://github.com/yaad-index/bonyan/commit/5cc035e97ecf7bb14340d5271e8b3de429faa5cf))

## [0.2.0](https://github.com/yaad-index/bonyan/compare/v0.1.0...v0.2.0) (2026-10-06)


### Features

* optional sampling temperature on chat requests ([#76](https://github.com/yaad-index/bonyan/issues/76)) ([767e99c](https://github.com/yaad-index/bonyan/commit/767e99c5c2dc4f07d3719c0c18ccfca1f446e976))

## 0.1.0 (2026-10-03)


### Features

* **agent:** let a run waiting on approval survive a restart ([#50](https://github.com/yaad-index/bonyan/issues/50)) ([5b970fa](https://github.com/yaad-index/bonyan/commit/5b970fa08e4c8d6144dd9c99216de382dd74d203))
* **agent:** memory in the run ([#35](https://github.com/yaad-index/bonyan/issues/35)) ([2e22bc3](https://github.com/yaad-index/bonyan/commit/2e22bc3c3d02e9421b0a804da3bff3d79a1c4373))
* **agent:** the agent loop ([#20](https://github.com/yaad-index/bonyan/issues/20)) ([909ecf1](https://github.com/yaad-index/bonyan/commit/909ecf186c361d4d47b39e17f7708761aa3c0741))
* **approval:** approvals for tool calls ([#34](https://github.com/yaad-index/bonyan/issues/34)) ([3b815d3](https://github.com/yaad-index/bonyan/commit/3b815d30071083b45c961747bdad004f14965fc2))
* **approval:** let each pending approver decide on its own ([#54](https://github.com/yaad-index/bonyan/issues/54)) ([8501695](https://github.com/yaad-index/bonyan/commit/850169558e4c5e7fed207ad978de60d36d046953))
* **assemble:** context assembly with per-section budgets ([#23](https://github.com/yaad-index/bonyan/issues/23)) ([13a0313](https://github.com/yaad-index/bonyan/commit/13a0313a063a3dbc15db32aeb0b1e054dccc6068))
* **budget:** price table, pre-call bound and charging from usage ([#13](https://github.com/yaad-index/bonyan/issues/13)) ([ba41d55](https://github.com/yaad-index/bonyan/commit/ba41d551f817a26f3b22308ebea3f41ffed3bcec))
* **chatcompat:** chat-completions adapter and model.Retry ([#19](https://github.com/yaad-index/bonyan/issues/19)) ([d04654a](https://github.com/yaad-index/bonyan/commit/d04654a5a4ccf43a0131532167ffd54f3af00aa7))
* core types for content, models, limits and outcomes ([#7](https://github.com/yaad-index/bonyan/issues/7)) ([d8d29d2](https://github.com/yaad-index/bonyan/commit/d8d29d26fb1e98c7a768d56e9943a33f941893b8))
* **eval:** evaluators as a registry slot ([#39](https://github.com/yaad-index/bonyan/issues/39)) ([3f109e7](https://github.com/yaad-index/bonyan/commit/3f109e7cf7e36b93a75b42664900776ac7feb7a8))
* **eval:** judge groundedness and unused context with a model ([#44](https://github.com/yaad-index/bonyan/issues/44)) ([22ab62f](https://github.com/yaad-index/bonyan/commit/22ab62f8d512ebe6a218d86be9d51787502b905f))
* **eval:** live evaluation through a queue ([#40](https://github.com/yaad-index/bonyan/issues/40)) ([c7d55b4](https://github.com/yaad-index/bonyan/commit/c7d55b422cab6f84e9fa258d58da9c1efb22a033))
* **eval:** re-runs of a recorded input ([#41](https://github.com/yaad-index/bonyan/issues/41)) ([018359b](https://github.com/yaad-index/bonyan/commit/018359b1ee0c98f8e35e29a8c1ee33fa62f8159a))
* **eval:** the eval runner and the deterministic evaluators ([#37](https://github.com/yaad-index/bonyan/issues/37)) ([dfabf6e](https://github.com/yaad-index/bonyan/commit/dfabf6e9c7d9e667419d3c73ee4796bdbe92f1e2))
* **hook:** hook points with payloads, change and deny ([#22](https://github.com/yaad-index/bonyan/issues/22)) ([f4ce2b0](https://github.com/yaad-index/bonyan/commit/f4ce2b047fdb25c00027ffd4941d207932862a41))
* keep suspended runs and approvals in owner-only directories ([#51](https://github.com/yaad-index/bonyan/issues/51)) ([148b417](https://github.com/yaad-index/bonyan/commit/148b417cf894db623f56e4be4cd03ca07afeeae1))
* **memory/honcho:** add a backend over the external memory service ([#65](https://github.com/yaad-index/bonyan/issues/65)) ([692bcd3](https://github.com/yaad-index/bonyan/commit/692bcd33347aa55ff0f5f0b66253d07d90e844c1))
* **memory/sqlite:** the basic memory backend ([#31](https://github.com/yaad-index/bonyan/issues/31)) ([d38a8de](https://github.com/yaad-index/bonyan/commit/d38a8deadc158e3665d59f23de1c7d190347e28c))
* **memory:** classify a derived record as model output too ([#63](https://github.com/yaad-index/bonyan/issues/63)) ([3af8001](https://github.com/yaad-index/bonyan/commit/3af80014e6b4895524251948a84b6c9284619e11))
* **memory:** give memory a namespace that the store applies ([#60](https://github.com/yaad-index/bonyan/issues/60)) ([d787e4e](https://github.com/yaad-index/bonyan/commit/d787e4ecd2656079669db30e99d4c876c484463a))
* **memory:** keep the model's replies in session history ([#46](https://github.com/yaad-index/bonyan/issues/46)) ([5a08651](https://github.com/yaad-index/bonyan/commit/5a086518c4b00246fcc9f46be39c1b9ac0dd011b))
* **memory:** keep the tool server's name with a recalled fact ([#49](https://github.com/yaad-index/bonyan/issues/49)) ([b7b06f1](https://github.com/yaad-index/bonyan/commit/b7b06f189256efc522271349cb4072cb47457aa6))
* **memory:** scrub every write in the store ([#59](https://github.com/yaad-index/bonyan/issues/59)) ([fe6e163](https://github.com/yaad-index/bonyan/commit/fe6e163d2eaad8fe1c65b7df4846940720167ba9))
* **memorytest:** make the any-word recall check lexical backends' own ([#61](https://github.com/yaad-index/bonyan/issues/61)) ([1a827f7](https://github.com/yaad-index/bonyan/commit/1a827f709990d3a2444c1e6d830d7f0339b5a855))
* **memory:** the memory interface and its conformance suite ([#27](https://github.com/yaad-index/bonyan/issues/27)) ([b7f3403](https://github.com/yaad-index/bonyan/commit/b7f34032529c0d31b8d008ba51301eeffd21a0e6))
* **prompt:** versioned prompts recorded on every call ([#42](https://github.com/yaad-index/bonyan/issues/42)) ([2935950](https://github.com/yaad-index/bonyan/commit/2935950b41031cd8e79c3b3cdfded5e76b177381))
* **record:** mark the run's input in the recording ([#55](https://github.com/yaad-index/bonyan/issues/55)) ([c89e819](https://github.com/yaad-index/bonyan/commit/c89e8194963369042b0dce0115fd82a48b4f2ddf))
* **record:** recording format, file sink and replay ([#15](https://github.com/yaad-index/bonyan/issues/15)) ([3abf325](https://github.com/yaad-index/bonyan/commit/3abf325867bddd21d71240835315473fdf213277))
* **record:** runs, tool failures and output retries in recordings ([#36](https://github.com/yaad-index/bonyan/issues/36)) ([5322867](https://github.com/yaad-index/bonyan/commit/532286708cc93b9f1e18b6db33c0b452f5d9498a))
* **registry:** make the approval store and the run store slots ([#52](https://github.com/yaad-index/bonyan/issues/52)) ([6b8bdb0](https://github.com/yaad-index/bonyan/commit/6b8bdb078908ec9ffec483be3fa9f3178ac9b93c))
* **registry:** named implementations per slot, assembled behind guards ([#11](https://github.com/yaad-index/bonyan/issues/11)) ([d8c1862](https://github.com/yaad-index/bonyan/commit/d8c18624b47bf81c8cd7cae16d394b664124171a))
* **registry:** record calls and events through the recording slot ([#16](https://github.com/yaad-index/bonyan/issues/16)) ([ff6beaa](https://github.com/yaad-index/bonyan/commit/ff6beaa87d5c141b92edb8654b582ed999e247ef))
* **secret:** sources, scoped resolver and scrubber ([#12](https://github.com/yaad-index/bonyan/issues/12)) ([d59dbf3](https://github.com/yaad-index/bonyan/commit/d59dbf30b9e07468b408d2c46ca5cb38e5027e70))
* **telemetry:** spans and metrics for runs, steps, model and tool calls ([#29](https://github.com/yaad-index/bonyan/issues/29)) ([06e28e7](https://github.com/yaad-index/bonyan/commit/06e28e7e92e136fb71fa9fa17200a7a7a4176064))
* **tokenize/tiktoken:** exact counter in its own module ([#14](https://github.com/yaad-index/bonyan/issues/14)) ([5f079a8](https://github.com/yaad-index/bonyan/commit/5f079a8ac6ad4dcf06af98559350b2798226155a))
* **tool:** register a tool server's tools ([#32](https://github.com/yaad-index/bonyan/issues/32)) ([943220a](https://github.com/yaad-index/bonyan/commit/943220af3244153ee441f084f1fa8af0838cac77))
* **tool:** the tool registry and structured output ([#25](https://github.com/yaad-index/bonyan/issues/25)) ([25bc26b](https://github.com/yaad-index/bonyan/commit/25bc26bae4c03bddc73f359aaec40bbf985cc7e2))
* **trust:** apply the trust policy in the run ([#24](https://github.com/yaad-index/bonyan/issues/24)) ([a73238b](https://github.com/yaad-index/bonyan/commit/a73238b77150ad0193f4b570bad3eda58929acc7))
* **trust:** name the tool server in remote tool output's source ([#48](https://github.com/yaad-index/bonyan/issues/48)) ([52f0437](https://github.com/yaad-index/bonyan/commit/52f0437e391a8c6f28f888d4f24836d63e9eff59))


### Bug Fixes

* **agent:** scrub events before they reach memory ([#58](https://github.com/yaad-index/bonyan/issues/58)) ([026584a](https://github.com/yaad-index/bonyan/commit/026584a3d768e065f17c5f9ff7e7679a2c1e0610))
* **eval:** re-run the recorded run's own message ([#43](https://github.com/yaad-index/bonyan/issues/43)) ([bed7383](https://github.com/yaad-index/bonyan/commit/bed7383b387effa61a01de9666a98f779cfc8edf))
