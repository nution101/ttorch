# Changelog

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
