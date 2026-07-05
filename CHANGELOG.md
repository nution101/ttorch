# Changelog

## [0.16.0](https://github.com/nution101/ttorch/compare/v0.15.0...v0.16.0) (2026-07-05)


### Features

* **model:** cheaper default tiers — drop ultracode default, tier manual spawns, sonnet manager ([c935a03](https://github.com/nution101/ttorch/commit/c935a0389875b5334b1413906df5216848b215a8))
* **model:** cheaper default tiers + retry escalation; bundle ponytail skill ([464892e](https://github.com/nution101/ttorch/commit/464892e8299a21e0b4f98c08b31c644eb5822c01))
* **model:** escalate the tier on retry, with fable as the top rung ([a00ac63](https://github.com/nution101/ttorch/commit/a00ac63c278522e20b82e779899b88afa685cef1))
* **skills:** bundle the ponytail agent skill, minimal-code worker default ([32ed80f](https://github.com/nution101/ttorch/commit/32ed80fc75ec009c66fa5dd36b3eb81c749d3e3f))

## [0.15.0](https://github.com/nution101/ttorch/compare/v0.14.0...v0.15.0) (2026-07-03)


### Features

* **model:** per-task model dial with dispatch-time complexity tiering ([#36](https://github.com/nution101/ttorch/issues/36)) ([e29c5e6](https://github.com/nution101/ttorch/commit/e29c5e65349b3dc4b94dbbd39574c2f9e674bc0d))

## [0.14.0](https://github.com/nution101/ttorch/compare/v0.13.0...v0.14.0) (2026-06-30)


### Features

* **codegraph:** opt-in, default-off worker code-navigation ([ab4da1e](https://github.com/nution101/ttorch/commit/ab4da1e54d2437771ba99c257daefa6914d67f84))
* **scheduler:** activate manager-window API-stall recovery ([c4516c0](https://github.com/nution101/ttorch/commit/c4516c0b7cbe437a278ce647009970e4f6916197))


### Bug Fixes

* **worktree:** never recycle a pool slot that still holds unlanded work ([fda599d](https://github.com/nution101/ttorch/commit/fda599dbf7f01913e488d2cffd1ed2717faa5889))

## [0.13.0](https://github.com/nution101/ttorch/compare/v0.12.0...v0.13.0) (2026-06-30)


### Features

* **scheduler:** count H4 governor deferrals in the scheduler-status deferred counter ([19fcf3a](https://github.com/nution101/ttorch/commit/19fcf3a51590b980bd2f6b718d1483f0553de9be))


### Bug Fixes

* **gate:** diff trust-prep review against the branch's true base, not stale local main ([a57ad51](https://github.com/nution101/ttorch/commit/a57ad5185ae7b88c7f903234f1584d86ee2d6364))
* **scheduler:** classify dispatch failures; park permanent, back off transient ([bb9c989](https://github.com/nution101/ttorch/commit/bb9c989536ac21967c152e9e3e955065221369ad))
* **tmux:** dedupe windows on create so recovery never stacks a duplicate ([740933c](https://github.com/nution101/ttorch/commit/740933cfc1f19c239794696851c760608bc08a96))

## [0.12.0](https://github.com/nution101/ttorch/compare/v0.11.0...v0.12.0) (2026-06-30)


### Features

* **scheduler:** auto-recover API-stalled sessions (worker live; manager pending invariant) ([085f29d](https://github.com/nution101/ttorch/commit/085f29dc0c166de69506924a0e6bc462f5c2d5a1))
* **scheduler:** dispatch file-overlapping tasks in parallel; surface land-rebase conflicts ([f2fe980](https://github.com/nution101/ttorch/commit/f2fe9803f3ef930e12a8ca2f7d9ce8051c7b8383))
* **validate:** retry a severed-transport failure instead of failing the gate ([044672b](https://github.com/nution101/ttorch/commit/044672b748f7b6f5bef737934a9f9f91fbdee09d))


### Bug Fixes

* **cli:** show the enforced delivery mode in `project ls`, sync it on init ([0ee85fd](https://github.com/nution101/ttorch/commit/0ee85fd234e89e84c227f36f6afd257da2a9014d))

## [0.11.0](https://github.com/nution101/ttorch/compare/v0.10.0...v0.11.0) (2026-06-29)


### Features

* **scheduler:** add a load-aware dispatch backpressure governor ([436530c](https://github.com/nution101/ttorch/commit/436530c1f79fd226ca6ebbe77e85358f30922817))
* **scheduler:** add opt-in daemon gate-pass to take the manager off the steady-state land path ([d15cf2f](https://github.com/nution101/ttorch/commit/d15cf2f4c4b6ec2a72f5eecb78a4573d27f40ab0))
* **scheduler:** auto-nudge alive-but-idle workers in the supervise pass ([0722229](https://github.com/nution101/ttorch/commit/07222295348160523d30de099ea71b07099d7b38))
* **scheduler:** make a stalled or idle daemon observable (heartbeat + status row + status cmd) ([db8253f](https://github.com/nution101/ttorch/commit/db8253f981cb1884ae4103bc995df6348f2be5dd))


### Bug Fixes

* **gate:** make the durable verdict the single merge authority; stop gated tasks retry-looping on token expiry ([d599c85](https://github.com/nution101/ttorch/commit/d599c85915a749b4bcb859f97b5df2c74554c871))
* **orchestrator:** fail closed when the occupancy/overlap board can't be read ([bf0d369](https://github.com/nution101/ttorch/commit/bf0d36924bb67bee28a4a72f7d25b1e10085679f))
* **orchestrator:** refresh the lease anchor on resume ([539d02b](https://github.com/nution101/ttorch/commit/539d02bb1128fdfef04285f8ee14a30faafad26b))
* **orchestrator:** refresh the window-gone anchor on resume ([c16dcde](https://github.com/nution101/ttorch/commit/c16dcde2e4608efe4c79cdbb08bdbda2079c5528))
* **scheduler:** require a stored brief before auto-dispatching a task ([a3ff7bc](https://github.com/nution101/ttorch/commit/a3ff7bc553453800bd817c74e7a75749d745cbbe))
* **scheduler:** stop the window-gone fast path repeat-reclaiming a re-dispatched worker ([1281676](https://github.com/nution101/ttorch/commit/12816769a248017ad37eba989706dbbba323fe64))
* **watch:** harden the singleton instance token against pane-pid reuse ([6399b4a](https://github.com/nution101/ttorch/commit/6399b4a8366af26148291cf5afb73537ba970e04))


### Performance Improvements

* **scheduler:** snapshot the live fleet once per dispatch tick ([fab69d8](https://github.com/nution101/ttorch/commit/fab69d8a1eac4bb92845e90f9f3b6cbcf469209e))

## [0.10.0](https://github.com/nution101/ttorch/compare/v0.9.0...v0.10.0) (2026-06-29)


### Features

* **scheduler:** auto-recover crashed/stalled workers with bounded restarts (--supervise) ([29f427b](https://github.com/nution101/ttorch/commit/29f427baa0c9cfdba5cc20522af206cef91d12c2))
* **scheduler:** make autonomy the default — auto-start + land fixes + task briefs ([f054769](https://github.com/nution101/ttorch/commit/f054769445d55dd09133b4e930661026c5d7ac39))

## [0.9.0](https://github.com/nution101/ttorch/compare/v0.8.0...v0.9.0) (2026-06-28)


### Features

* **land:** carry the approval forward with the verdict on a clean rebase ([87541ba](https://github.com/nution101/ttorch/commit/87541bad25bbe39e3a830b8fd98c5236ef1c9f64))
* **scheduler:** autonomously land already-gated work (--land) ([3b52e86](https://github.com/nution101/ttorch/commit/3b52e869c4bc0a0f6b2ceae7b6a88a6b17f3b744))
* **worker:** scale reasoning effort to task complexity (--effort) ([9534067](https://github.com/nution101/ttorch/commit/95340671ae246e6205ddc4e0ed8cdb58b5d59cb5))

## [0.8.0](https://github.com/nution101/ttorch/compare/v0.7.0...v0.8.0) (2026-06-28)


### Features

* **scheduler:** add the deterministic dispatch daemon (roadmap A, phase 1) ([bab7af4](https://github.com/nution101/ttorch/commit/bab7af4f007dba4da3aa03c564fe8f0737ffafe7))

## [0.7.0](https://github.com/nution101/ttorch/compare/v0.6.0...v0.7.0) (2026-06-28)


### Features

* **db:** add durable task leases + reclaim to the tasks table ([cdb7eec](https://github.com/nution101/ttorch/commit/cdb7eeccf478368713745d78bc9160b5bd4d8678))
* **gate:** make the trust-gate verdict durable in SQLite ([2e3b9a4](https://github.com/nution101/ttorch/commit/2e3b9a44c6d21fa8a4ed94d0f11fc1ade3d3d7a7))
* **livestate:** broaden recoverable API-stall auto-resume patterns ([125562b](https://github.com/nution101/ttorch/commit/125562b40d2633b4bd4fdacbf9ab342575f59a41))
* **orchestrator:** scale the trust gate's reviewer set to diff size ([1aefa73](https://github.com/nution101/ttorch/commit/1aefa73e21926f19a053fc4c87bb981c89383b44))
* **review:** classify diff size to scale the reviewer set ([3d5524e](https://github.com/nution101/ttorch/commit/3d5524ecf36003bb178fed9067b6748a7891e049))
* **watch:** add an external manager-liveness watchdog ([a567d2c](https://github.com/nution101/ttorch/commit/a567d2c41b8ec741b812408403e4c070a35d521c))


### Bug Fixes

* **manager:** enforce disjoint dispatch and an always-armed wake ([187e6cf](https://github.com/nution101/ttorch/commit/187e6cfeefaa4de1f41af46feff90a3df6ce85bc))
* **manager:** point capacity guidance at the new `free slots` signal ([4c09f88](https://github.com/nution101/ttorch/commit/4c09f88d173e00eaad6b335d7d83b0a53d9a11c9))
* **review:** classify diff size off an authoritative unquoted file list ([0c74da3](https://github.com/nution101/ttorch/commit/0c74da3ef801279bdd94494f52e238f707451603))
* **status:** report real worktree-pool capacity, not idle-worker count ([91f24ae](https://github.com/nution101/ttorch/commit/91f24aec4b7c34f84b7c4e5efc472103e9036a41))

## [0.6.0](https://github.com/nution101/ttorch/compare/v0.5.2...v0.6.0) (2026-06-28)


### Features

* **cli:** land a done set concurrently via 'ttorch land --all' ([83404fe](https://github.com/nution101/ttorch/commit/83404fe20cfc92e47f8b2e25dd31d81d41194746))
* **db:** add the auto_resumed event type for watcher-driven resume ([033bd8e](https://github.com/nution101/ttorch/commit/033bd8e680f6e41d5ec71ad1be28e035e8c9df83))
* **livestate:** add Stalled heuristic for the mid-stream API-stall error ([6ddf747](https://github.com/nution101/ttorch/commit/6ddf747e020cbf146ba80714319ef62e0a4013e3))
* **orchestrator:** async pipelined land queue (LandSet) ([5ef092c](https://github.com/nution101/ttorch/commit/5ef092c221e92e555d28c8fafb7d5d992785c179))
* **orchestrator:** carry a verdict forward over a clean rebase ([f69c0dc](https://github.com/nution101/ttorch/commit/f69c0dc3bcca4a5745f28ff3423ae4da39a261ab))
* **review:** record a content identity on the verdict ([f4c5c47](https://github.com/nution101/ttorch/commit/f4c5c471715c9a1927c0b6585175168403a11508))
* **watch:** auto-resume an API-stalled worker on the idle liveness path ([24e0b13](https://github.com/nution101/ttorch/commit/24e0b131478b692074f6a97ffa0f812201a02232))


### Bug Fixes

* **termtab:** pin worker-view sessions destroy-unattached off ([327fa6e](https://github.com/nution101/ttorch/commit/327fa6e2cd1fbd53eceaf475c38cba650b3768b6))
* **tmux:** pin destroy-unattached off on the shared session ([6ab351a](https://github.com/nution101/ttorch/commit/6ab351a8baadd1c62f18b3b1c7c52d9f69fa5af2))


### Performance Improvements

* **orchestrator:** reuse trust prep's validate at an unchanged-HEAD merge ([64ad52e](https://github.com/nution101/ttorch/commit/64ad52e9afd82461291946beee431bc42c6b0e60))
* **review:** advisory qa reviewer trusts the staged validate.json ([0d1a270](https://github.com/nution101/ttorch/commit/0d1a270839d59539e438ff9618c41043ffa958b3))

## [0.5.2](https://github.com/nution101/ttorch/compare/v0.5.1...v0.5.2) (2026-06-27)


### Bug Fixes

* **trust:** refuse a stale base and stage a three-dot review diff in trust prep ([12b5df9](https://github.com/nution101/ttorch/commit/12b5df9951616ff5f553740de994081f67376911))
* **watch:** reap an orphan holding the singleton instead of exiting blind ([b8c06f1](https://github.com/nution101/ttorch/commit/b8c06f18ed4a20e8c85cc88e9527834019166707))
* **worktree:** base pooled worktrees on the fresh origin default ([a22f043](https://github.com/nution101/ttorch/commit/a22f0434578e6f6b89b1607b4f40fe6a1077f746))


### Performance Improvements

* **review:** trust-gate reviewers trust the staged validate.json ([74596b7](https://github.com/nution101/ttorch/commit/74596b7998c10e2d2f8770bd1996e1984b66bf31))

## [0.5.1](https://github.com/nution101/ttorch/compare/v0.5.0...v0.5.1) (2026-06-26)


### Bug Fixes

* **install:** verify cosign signatures strictly by default ([403d27c](https://github.com/nution101/ttorch/commit/403d27c115a49287da5c38783b30b4c3552a2064))
* **spawn,teardown:** guard committed-but-unmerged work; add spawn --brief-file ([082492a](https://github.com/nution101/ttorch/commit/082492a822754aae150ba9d8df2a65f36d4da621))
* **watch:** gate idle_unreported on a wall-clock dwell ([d75a4c4](https://github.com/nution101/ttorch/commit/d75a4c4a5ba5751e56e961e2150f3d5c797a58b6))
* **worker:** wait silently for the brief during the spawn gap ([bf22f84](https://github.com/nution101/ttorch/commit/bf22f845f6a09fd1b0564ea01f17303b7d357a6a))

## [0.5.0](https://github.com/nution101/ttorch/compare/v0.4.0...v0.5.0) (2026-06-26)


### Features

* **cli:** wire the advisory qa-review audit (orchestrator + CLI) ([aa90808](https://github.com/nution101/ttorch/commit/aa908084bbb218e865dbc792a27221b6f264b0af))
* **install:** cosign-verify release checksums in install.sh ([95dfa52](https://github.com/nution101/ttorch/commit/95dfa52f2fd2dec3b4dc957a0a830aab3b590e3d))
* **orchestrator:** restore auto-init by default, tracked-file-safe ([b885488](https://github.com/nution101/ttorch/commit/b885488953bfb7ae642d09a9db10ccf07ede4848))
* **review:** add advisory QA test-adequacy reviewer and convention worker guidance ([b75f91d](https://github.com/nution101/ttorch/commit/b75f91db206264dcd89b74924e70dd47cf060458))


### Bug Fixes

* **watch:** isolate tests from an ambient TTORCH_DB ([f70dfe8](https://github.com/nution101/ttorch/commit/f70dfe8b90342d5ed681d6c93f46f51b4072361a))

## [0.4.0](https://github.com/nution101/ttorch/compare/v0.3.0...v0.4.0) (2026-06-26)


### Features

* **cli:** DB-backed query surfaces (inc4) ([55b0a60](https://github.com/nution101/ttorch/commit/55b0a605d5edbf4bd25728c72db2b380e46ed99b))
* **cli:** worker-reporting commands + spawn task identity ([b79aa96](https://github.com/nution101/ttorch/commit/b79aa96df18d6190dd6becca2f985191aa214385))
* **db:** add SQLite state-store foundation and relocate Busy ([7dcc600](https://github.com/nution101/ttorch/commit/7dcc600b8b35862daa01b3af169124bc011c293a))
* **db:** flip persistence to the SQLite store + legacy import ([df09c08](https://github.com/nution101/ttorch/commit/df09c0839236884606b74f6d4a11a25c8c0d5d6e))
* **lifecycle:** record typed lifecycle events + db/teardown cleanups (inc5) ([4390fd0](https://github.com/nution101/ttorch/commit/4390fd09a19b05c4d91d4ae60c9e7719323c6aed))
* **manager:** rewrite the protocol to the event-driven watch loop (inc7) ([d1e5bfb](https://github.com/nution101/ttorch/commit/d1e5bfb99ebf76be7835b1b27cf78f9b419b6267))
* **supervisor:** retire the daemon, wake-queue, and manager injection (inc6) ([dead982](https://github.com/nution101/ttorch/commit/dead9828cb8375c83e82e705b0e53e9aa03b0624))
* **watch:** event-driven `ttorch watch` (inc3) ([5cd4e12](https://github.com/nution101/ttorch/commit/5cd4e126dc82c6b490a58fe4c727ba66776d5dfc))


### Bug Fixes

* **cli:** make worker audit attribution unforgeable ([fd54f42](https://github.com/nution101/ttorch/commit/fd54f42ec429b87b6a6c8d876691936fc25742b7))
* **cli:** status shows only spawned workers, not pending backlog ([f1dfc78](https://github.com/nution101/ttorch/commit/f1dfc788c7e7a608b10a75e3623c5a52fb29ac6e))

## [0.3.0](https://github.com/nution101/ttorch/compare/v0.2.0...v0.3.0) (2026-06-25)


### Features

* **ciparity:** reproduce a repo's actual CI run-steps locally ([7b78f6c](https://github.com/nution101/ttorch/commit/7b78f6cd3a5ab13ab4b631febcfc3ae6a999e19a))
* **manager:** add a diagnose-from-evidence guardrail ([b84945c](https://github.com/nution101/ttorch/commit/b84945c5e2e667d845135258f9a726ec22ec397b))
* **manager:** mirror the diagnose-from-evidence guardrail into global guidance ([3bf2e49](https://github.com/nution101/ttorch/commit/3bf2e49526ea5c8731bea2729edbee642022faeb))
* **review:** run the security audit in every delivery mode ([bec1088](https://github.com/nution101/ttorch/commit/bec108866445f28c72de87ea79a607fd55577d34))


### Bug Fixes

* **ciparity:** broaden host-mutation skips and surface all env scopes ([544b608](https://github.com/nution101/ttorch/commit/544b60896995fa1ee96004e0b60993f5ec5d38fc))
* **ciparity:** gate auto-run with a fail-closed allowlist ([a0674df](https://github.com/nution101/ttorch/commit/a0674df1e3251ed5824ef00681d1a5494364ba89))
* **ciparity:** reject path-qualified executables and go exec flags ([7f06364](https://github.com/nution101/ttorch/commit/7f06364a188e90878abe603e3abe5d6f582112b7))
* **ciparity:** treat leading inline VAR=val assignments as unknown ([a8bdafb](https://github.com/nution101/ttorch/commit/a8bdafbb69372935a312f9e1d3a9392cb9d37654))
* **supervisor:** stop the heartbeat from poking the manager ([5d90850](https://github.com/nution101/ttorch/commit/5d90850a50729640db87dcf0988befe5ca00fa5b))

## [0.2.0](https://github.com/nution101/ttorch/compare/v0.1.10...v0.2.0) (2026-06-25)


### Features

* add 'trusted' delivery mode, a mode reader, and merge provenance ([bd30057](https://github.com/nution101/ttorch/commit/bd300579924a719b5e0d21f21a15f7f433db2527))
* add 'ttorch land &lt;id&gt;' for one-command safe delivery ([2bcaea9](https://github.com/nution101/ttorch/commit/2bcaea9500209b2a075afe9e8215ccb88bb97136))
* add 'ttorch trust' producer (prep/record/show); merge gate unchanged ([4dc7fd2](https://github.com/nution101/ttorch/commit/4dc7fd27e958d7c6992dd10da8dadb54e8fec56e))
* add curated coding-agent profiles to managed content ([108ed5c](https://github.com/nution101/ttorch/commit/108ed5cebf9f7fb0c8070fee06ca6978a35ac0a3))
* add curated coding-agent profiles to managed content ([d74d80d](https://github.com/nution101/ttorch/commit/d74d80d3996f6ab31f1adf9673f1e548d094f6f4))
* add the adversarial-review verdict package (foundation) ([3187d3f](https://github.com/nution101/ttorch/commit/3187d3f27aa9a00982a4b279de073812606f6cda))
* add ttorch land — atomic rebase + validate + merge + verify ([1785b16](https://github.com/nution101/ttorch/commit/1785b164c0f0febc41374fbe1981c7aa8ecd1cc4))
* adversarial-review trust gate — foundation (inert) ([dbb28e8](https://github.com/nution101/ttorch/commit/dbb28e88c56ddd7643fc86bcdbc9f07728074c14))
* **cli:** make `ttorch send` deliver arbitrary message text safely ([fd93515](https://github.com/nution101/ttorch/commit/fd935154bb076ceb628ca76fa6664c75f4a83168))
* enforce the adversarial-review trust gate (trusted mode, off by default) ([5f57d9d](https://github.com/nution101/ttorch/commit/5f57d9d532f9d0237300e52d574445da5218124a))
* enforce the adversarial-review trust gate in merge-local ([2148466](https://github.com/nution101/ttorch/commit/2148466bf4be2a427f555252958b1f2e0867b900))
* expand curated coding-agent profiles with language and specialist agents ([9e1a9f5](https://github.com/nution101/ttorch/commit/9e1a9f576ed7bc0ac76ccd718b4f0266833fb5d8))
* expand managed agent roster (languages + finance-relevant specialists) ([f5ce244](https://github.com/nution101/ttorch/commit/f5ce2442d418a95a4b770e63d52532e8a217a94b))
* footprint-based spawn conflict prevention (deterministic disjoint dispatch) ([ff31773](https://github.com/nution101/ttorch/commit/ff3177331ea3a85ec5bfde2f9617d6ef0caee909))
* global ~/.claude/settings.json hook-installer mechanism ([30513c0](https://github.com/nution101/ttorch/commit/30513c09936f16bdd3151a02763adcff58a83bd7))
* **installer:** merge a ttorch-managed block into global settings.json ([75c8201](https://github.com/nution101/ttorch/commit/75c82010e93e7301549f6f4ce6b93528ad9c400d))
* **installer:** ship a safe, advisory UserPromptSubmit hook ([2752b3b](https://github.com/nution101/ttorch/commit/2752b3bf4e51f311aebec36e8db1b635a2a3855c))
* manager operating rules (board-as-truth, fleet, autonomy loop) in SKILL + charter ([4ba4ef0](https://github.com/nution101/ttorch/commit/4ba4ef0cd7ea4f6e4352c1217a58665bb1158e65))
* **manager:** reframe the manager loop around a live board and a moving fleet ([b642d21](https://github.com/nution101/ttorch/commit/b642d214d4f30cddb0ae5f0768bd8163568c3899))
* prevent dispatching parallel workers onto overlapping files ([b748e71](https://github.com/nution101/ttorch/commit/b748e718327522fa0e9d33de98547d1b3c7d11a9))
* ship advisory prompt-reminders hook via the global-settings installer ([c62ca6d](https://github.com/nution101/ttorch/commit/c62ca6d24009f3ed62f9148d904089384ead2ca6))
* ship the adversarial-review reviewer content and trusted-mode carve-out ([33385ec](https://github.com/nution101/ttorch/commit/33385ec405b5a91f3aa4d651791e7524b36428f2))
* show friendly names on tmux/iTerm tabs instead of 'tmux N' ([a2ff3c5](https://github.com/nution101/ttorch/commit/a2ff3c58eb69487d056b66cfb172153b270b3bc7))
* show friendly names on tmux/iTerm tabs instead of 'tmux N' ([8698864](https://github.com/nution101/ttorch/commit/869886476b4171c700b35c92d0b688206fc20cef))
* supervisor auto-drives the manager (poke on actionable wakes + heartbeat) ([432d5bf](https://github.com/nution101/ttorch/commit/432d5bfcf09c13868082ce7f0e595193a1e237e2))
* supervisor sets live working/idle tab glyphs ([96713f3](https://github.com/nution101/ttorch/commit/96713f3a36a31b242e4b9111ea87b8be7cfec9d7))
* **supervisor:** color worker tabs by live state ([0548ddf](https://github.com/nution101/ttorch/commit/0548ddfd5995bd6fb21b234129d4a62084dc3d01))
* **supervisor:** poke the manager on actionable wakes so it never stalls ([3c2359a](https://github.com/nution101/ttorch/commit/3c2359a63d2e14e2273265c6405ea88eabe929c1))


### Bug Fixes

* gate operates on the committed object, never the mutable worktree ([f000e2c](https://github.com/nution101/ttorch/commit/f000e2c41b5c8698b9621eba3c99eabbeaa5c8f4))
* harden spawn-reliability against review findings ([38292c9](https://github.com/nution101/ttorch/commit/38292c9016005e47c86c858bd4b06c6f24c2f06b))
* harden the trust gate against fail-open and worker-controlled validation ([1025e4b](https://github.com/nution101/ttorch/commit/1025e4beee87da574c62fd1b91d14d9a95e460f5))
* make the supervisor singleton claim atomic with an advisory lock ([1972f6a](https://github.com/nution101/ttorch/commit/1972f6a3627f8060ab815650ba6c269e54e52628))
* make the supervisor singleton claim atomic with an advisory lock ([ac4fdc0](https://github.com/nution101/ttorch/commit/ac4fdc0ccda2e0d52dd1e4b2f5eadea0f2aba253))
* make ttorch spawn read-only w.r.t. tracked files ([3b8362b](https://github.com/nution101/ttorch/commit/3b8362bdddf4b5b6834e80e3b78210887c06e6e6))
* make ttorch spawn read-only w.r.t. tracked files ([a848c1c](https://github.com/nution101/ttorch/commit/a848c1cfee5d0e4babf1c794e3c41a780dd899fb))
* make worker spawn reliable — ready-gate the brief and start on a fresh branch ([4daa9e5](https://github.com/nution101/ttorch/commit/4daa9e54e428c74c9623577188f533c8136e2712))
* spawn reliability — brief-delivery readiness + fresh branch on worktree reuse ([c10abe0](https://github.com/nution101/ttorch/commit/c10abe0365353329a6732099d958e926731e5676))
* tell the manager when a worker finishes a turn or goes idle ([bf6231a](https://github.com/nution101/ttorch/commit/bf6231ab46e88e699aac4a5839de74d73518b811))
* tell the manager when a worker finishes a turn or goes idle ([8f59b6b](https://github.com/nution101/ttorch/commit/8f59b6bdbbaa661e8e94f7596ffce1d5e71d8cd4))
* trusted auto-merge requires a default-branch gate script ([d33d2bb](https://github.com/nution101/ttorch/commit/d33d2bb402e372280b18ed8106a9e9dc2c4381a4))
* ttorch send accepts arbitrary text safely (stdin/file, verbatim, fail-loud) ([a861a28](https://github.com/nution101/ttorch/commit/a861a2876f90ca09a9df481ac6aef916b57578bc))
* **worktree:** supply a fallback committer identity for rebase ([83ca436](https://github.com/nution101/ttorch/commit/83ca436e3d288dc15cc97c3705ba088765779e46))
* **worktree:** supply a fallback committer identity for rebase ([cdce633](https://github.com/nution101/ttorch/commit/cdce633bbb6e5b84aa468f40f2b8716d120762f7))
