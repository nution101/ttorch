# Changelog

## 1.0.0 (2026-10-03)


### Features

* **approve:** refuse a non-interactive or worker-context approval ([021773e](https://github.com/nution101/ttorch/commit/021773e16174dbf0ea5568c5b03bf65626a8d1b6))
* autonomous trusted-gating + tree-hash validate cache ([437a0d5](https://github.com/nution101/ttorch/commit/437a0d564dacd5ff17f75c66958dcb7b9acc9f24))
* **backend:** add a session-backend interface with a tmux implementation ([6a5bcfb](https://github.com/nution101/ttorch/commit/6a5bcfbbce8342492708f9a19a914ebc26bfd7b4))
* **backend:** add PanePIDErr to the session backend ([24f817a](https://github.com/nution101/ttorch/commit/24f817a42cf452b56e1ec2e0860f763e0349a87a))
* **backend:** add TypeLine and SendKey to the session backend ([ad1ece1](https://github.com/nution101/ttorch/commit/ad1ece16fd044d18bf1a8f0f3d06ace49e130ea6))
* **board:** serve pending decisions and fleet state on a local page ([ae702f6](https://github.com/nution101/ttorch/commit/ae702f60d86d797be09b9cbbc15e16e0edc1883c))
* **brieflint:** add --offline, so an unreachable remote costs one rule ([57f91c1](https://github.com/nution101/ttorch/commit/57f91c175965f2b42c68e825f4c95000d443204b))
* **brieflint:** check a task brief before it is stored ([fd35159](https://github.com/nution101/ttorch/commit/fd351598055f1617f54c48b298a1c45910471627))
* **brieflint:** give a reduced-coverage run its own exit status ([09699cf](https://github.com/nution101/ttorch/commit/09699cfc92c3a9f5ad1e87085a62d65adea38547))
* **cli:** add spawn --workdir to choose a clone or a worktree ([5f260cc](https://github.com/nution101/ttorch/commit/5f260cc367fe98e88fb13481e7b48f95a819f0c2))
* **cli:** add ttorch board ([35a7ce6](https://github.com/nution101/ttorch/commit/35a7ce6d3ef89071d3e3eb9ceb85a19dbee637a8))
* **cli:** add ttorch brief-lint ([eb2cb5f](https://github.com/nution101/ttorch/commit/eb2cb5f08c4ffc30e22fcb4cd3b153d35625d42e))
* **cli:** add ttorch escalate, decisions and answer ([d5cae29](https://github.com/nution101/ttorch/commit/d5cae29f84e79c7e07226e9f09a1fda4b1de4cc5))
* **cli:** add ttorch peer add and ttorch peer adopt ([d1d32c7](https://github.com/nution101/ttorch/commit/d1d32c71a32fe69dafe9ae56e26da657442bb168))
* **cli:** add ttorch peer init ([61e9cf2](https://github.com/nution101/ttorch/commit/61e9cf24488db15eb6a8a80126bcedd44c3c07c9))
* **cli:** add ttorch peer serve ([84de3e3](https://github.com/nution101/ttorch/commit/84de3e3902c552a061d2e0e388f8feda3b44a784))
* **cli:** add ttorch summary with a --json form ([aa98ba2](https://github.com/nution101/ttorch/commit/aa98ba2f71a78602539cbc2f94afc583e481cc5a))
* **cli:** add ttorch sync to refresh a clone worker's base ([096aa6e](https://github.com/nution101/ttorch/commit/096aa6ea0282cb10eef4923e4a007215ed8eaed7))
* **cli:** announce the gate-change approval default in update and doctor ([cc5e65b](https://github.com/nution101/ttorch/commit/cc5e65baea12055d5c92216f6bafdf00fb158002))
* **cli:** lint a supplied brief at task add ([0a1dbcd](https://github.com/nution101/ttorch/commit/0a1dbcd5dfd35e0a4b2af18bc113aaece8b59915))
* **cli:** lint the brief spawn stores, as task add already did ([5878ae2](https://github.com/nution101/ttorch/commit/5878ae26dee38ce96e45ff484417e3c1dc70056d))
* **cli:** record harness lifecycle events with ttorch hook ([0d99d99](https://github.com/nution101/ttorch/commit/0d99d99c2a204f6c8227a37bb8843354e0638ec6))
* **cli:** refuse peer add on a coordinator that is itself a peer ([93c9527](https://github.com/nution101/ttorch/commit/93c95278fa0c647eda8b5032ad81adffd899b34c))
* **cli:** run the peer pass in the scheduler daemon over the control client ([93d812c](https://github.com/nution101/ttorch/commit/93d812c4df257354a884a9ed81f7c1279205bfbc))
* **cli:** use a provisioned peer through its control key ([55d8c88](https://github.com/nution101/ttorch/commit/55d8c88bab4f5795ca0bd3145b2292066e016d29))
* **clonepool:** provision a private clone per worker ([2b37c5b](https://github.com/nution101/ttorch/commit/2b37c5b389d772c39205931b862b9409bf1c63c1))
* **codegraph:** opt-in, default-off worker code-navigation ([ab4da1e](https://github.com/nution101/ttorch/commit/ab4da1e54d2437771ba99c257daefa6914d67f84))
* **db:** add a backlog task once per request id ([2dbfbac](https://github.com/nution101/ttorch/commit/2dbfbac3f2a55ee829510d3fd080cfd464a4d534))
* **db:** add a rate-limited hook_turn_started event ([1ca0966](https://github.com/nution101/ttorch/commit/1ca0966036a7660edf14015ddf102788659e9ec2))
* **db:** add an atomic inbox consume and a latest-event lookup ([3e7987f](https://github.com/nution101/ttorch/commit/3e7987f514814ddef14f883030f9af166aefb7f2))
* **db:** add an atomic one-time claim on the event spine ([8de1d76](https://github.com/nution101/ttorch/commit/8de1d768da7af457a6e84b2131b875be596b1252))
* **db:** add the coordinator, escalations and peer_requests tables ([000d095](https://github.com/nution101/ttorch/commit/000d0958686eb8de4861ac8a937b03ae69728d0b))
* **db:** add the peers, peer_repos and peer_delegations tables ([8c52dfc](https://github.com/nution101/ttorch/commit/8c52dfc34725856417f7db8a1a83fb055ed65e4d))
* **db:** open the state store read-only without migrating ([8bfa1ec](https://github.com/nution101/ttorch/commit/8bfa1ec5d3c552dbeb361810d1782afc48abc722))
* **db:** read a task's stall clock and ladder off the event spine ([e294648](https://github.com/nution101/ttorch/commit/e2946488281e7346d1e824b725ddb12a596f1f24))
* **db:** read recent task events of given types across all tasks ([b61ec50](https://github.com/nution101/ttorch/commit/b61ec500355dd09fe047601bc3a0a37321981731))
* **db:** read the coordinator row ([8b07b9c](https://github.com/nution101/ttorch/commit/8b07b9cebae62450a7fd1b2984960aae8cd37d4c))
* **db:** record a parent coordinator on the coordinator row ([026a75d](https://github.com/nution101/ttorch/commit/026a75df583142df0ed56fc1e0cd4091959997f9))
* **db:** record a peer poll and the events it raises in one transaction ([ec8c48d](https://github.com/nution101/ttorch/commit/ec8c48df683898bff4c633f09062c1d66259ace4))
* **db:** record each project's default branch, last land and review base ([209cce6](https://github.com/nution101/ttorch/commit/209cce6d9f1dcc6460f8b7a2e133e292c29141b8))
* **db:** record goals and answers from a parent coordinator ([37abdfd](https://github.com/nution101/ttorch/commit/37abdfdada06f2cf553bcf7cb49bc2bd497e59b4))
* **db:** register peers, their repositories and what was delegated to them ([cae4bf5](https://github.com/nution101/ttorch/commit/cae4bf5a0e1bab58b4d7e12fa1b7e042a05437de))
* **db:** store escalations, answer them once per request id ([5b3de2f](https://github.com/nution101/ttorch/commit/5b3de2f425c25337e1f68be42828f4341828f7be))
* **doctor:** check git against the 2.32 floor per-worker clones need ([cc50903](https://github.com/nution101/ttorch/commit/cc50903d7087ec1fcf4866626cd2f9def5134fb0))
* **doctor:** print what the trust gate reads for each trusted project ([3f35700](https://github.com/nution101/ttorch/commit/3f3570098e9ef321cfdc10c3956db2a8943985e0))
* **gate:** add the seam that tells a clone task from a worktree task ([7c816b1](https://github.com/nution101/ttorch/commit/7c816b180ee22885552068a408d2a0f9da0ef9bd))
* **gate:** bind a gate-change approval to the files it was granted for ([f149d1d](https://github.com/nution101/ttorch/commit/f149d1d0b4adf4b4b0f8edb98f221f12c63f9975))
* **gate:** content-address the validate cache by git tree hash ([ba1b124](https://github.com/nution101/ttorch/commit/ba1b1242ad46039dcaad44482ceba7761b7121ac))
* **gate:** cover CLAUDE.md, the symlink the guard matched by its own path ([9ad93f8](https://github.com/nution101/ttorch/commit/9ad93f88ea318fcff38259f16a7d32ed403e5378))
* **gate:** cover go.work, go.work.sum and vendor/ ([de19c56](https://github.com/nution101/ttorch/commit/de19c5684c29ca5dc70f6efd50a56c6c610f9d8c))
* **gate:** cover internal/skills/, the shortest route into ~/.claude/skills ([265e0e2](https://github.com/nution101/ttorch/commit/265e0e2ab7c7651c9e683a0257c5cb309b834895))
* **gate:** cover project agent config, go.mod/go.sum and nested instruction files ([84e749b](https://github.com/nution101/ttorch/commit/84e749b6feadcdf0ea46a0ac31d8d8b83b50996a))
* **gate:** cover the deciding code and CI in the gate-config guard ([d6896b2](https://github.com/nution101/ttorch/commit/d6896b2366c2f7948ef47edf5d6869fba1cb44fb))
* **gate:** cover the gate's own instructions in the gate-config guard ([9a2b8de](https://github.com/nution101/ttorch/commit/9a2b8dee82f2a39ddca30bd61b172007bf066ab6))
* **gate:** cover the whole content/ tree, not the subtrees the gate dispatches ([0c8baa6](https://github.com/nution101/ttorch/commit/0c8baa6c26891356ee56eeb049089996b0568b9c))
* **gate:** give a clone task's reviewer mirror no clone to fetch from ([f7d7b5d](https://github.com/nution101/ttorch/commit/f7d7b5db58e51a4022682aed6a6a22aa5ed1cbe0))
* **gate:** make .ttorch/ a prefix, closing the learnings write channel into AGENTS.md ([95147d3](https://github.com/nution101/ttorch/commit/95147d3f8a4631360220191dc47a126150020fee))
* **gate:** make the human gate-change approval opt-in ([aaf32b9](https://github.com/nution101/ttorch/commit/aaf32b91e48c7443e44f1345f5a583c6d2c7f1c4))
* **gate:** read a clone task's head and diffs in the project repository ([cef2a33](https://github.com/nution101/ttorch/commit/cef2a33a158a3dafe5a2dd983062491b2f653875))
* **gate:** require an explicit approval to merge a gate-definition change ([69309fc](https://github.com/nution101/ttorch/commit/69309fc761656ea3e9be890faf07d52c473c2525))
* **gate:** warn when the default branch no longer contains the last land ([beabc07](https://github.com/nution101/ttorch/commit/beabc0755c4c4eb8a92f1154eefca83b1fd42bfb))
* **harness:** give clone workers a git ceiling and a private global config ([6f96513](https://github.com/nution101/ttorch/commit/6f96513f42db5f998bfdab2b60807450e5f4dd3f))
* **harness:** wire Claude Code's lifecycle hooks into worker settings ([78b052e](https://github.com/nution101/ttorch/commit/78b052eb96e09f244c21c4d44c8f01353c46a441))
* **herdr:** add a Unix-socket client for Herdr's JSON API ([2c311fe](https://github.com/nution101/ttorch/commit/2c311fee5a82d7e73eef8188d7f79a135faa8381))
* **herdr:** add SendLiteral and warn that SendText and SendInput are raw ([d582a75](https://github.com/nution101/ttorch/commit/d582a75b988c62467d9c30f32db1f5a1b3fda5e5))
* **herdr:** add the worker-hosting calls and status subscription ([71bca67](https://github.com/nution101/ttorch/commit/71bca67e6dd5f47342ae1eda168aab06a7d74d25))
* **hook:** leave a hook_turn_started trace when a turn starts ([310058f](https://github.com/nution101/ttorch/commit/310058fd5839f975aab365f2a4aa312bc81842ab))
* **init:** report the gate-change approval setting for a trusted repo ([9fb3f28](https://github.com/nution101/ttorch/commit/9fb3f284cbca28dc9cee39ce474e36c48e264127))
* **installer:** carry model and effort across updates of a managed agent ([e3fb5eb](https://github.com/nution101/ttorch/commit/e3fb5eb1fde1c7d9b2959431a6d019a63bbdcbe7))
* **land:** merge the commit a clone task's land rebased ([2d40ac9](https://github.com/nution101/ttorch/commit/2d40ac915fd3bb314200df1afd20117835a4533e))
* **land:** print the unapproved gate-change line in the land summary ([aec2b49](https://github.com/nution101/ttorch/commit/aec2b49a0d89cd899c82555dbd9c4d0b4361a6de))
* **land:** rebase a clone task's head in a scratch worktree of the project ([121ab0e](https://github.com/nution101/ttorch/commit/121ab0e9060bde44432a2b3260b69cbf2c24c2b1))
* **livestate:** reconcile a hook turn record with the pane heuristic ([4687d69](https://github.com/nution101/ttorch/commit/4687d696fc7e87e7818fa60edfc32a50b4f814c4))
* **model:** cheaper default tiers — drop ultracode default, tier manual spawns, sonnet manager ([c935a03](https://github.com/nution101/ttorch/commit/c935a0389875b5334b1413906df5216848b215a8))
* **model:** cheaper default tiers + retry escalation; bundle ponytail skill ([464892e](https://github.com/nution101/ttorch/commit/464892e8299a21e0b4f98c08b31c644eb5822c01))
* **model:** escalate the tier on retry, with fable as the top rung ([a00ac63](https://github.com/nution101/ttorch/commit/a00ac63c278522e20b82e779899b88afa685cef1))
* **model:** per-task model dial with dispatch-time complexity tiering ([#36](https://github.com/nution101/ttorch/issues/36)) ([e29c5e6](https://github.com/nution101/ttorch/commit/e29c5e65349b3dc4b94dbbd39574c2f9e674bc0d))
* **orchestrator:** build a Manager over a store the caller opened ([8d6d14f](https://github.com/nution101/ttorch/commit/8d6d14f116248b55688cb4bd1a440412ea74b546))
* **orchestrator:** cover the authorized_keys line and peer init ([b9edeb0](https://github.com/nution101/ttorch/commit/b9edeb03ad309e8d406a354da03cdf0e863250ce))
* **orchestrator:** cover the peer control channel's verb list ([6b918a4](https://github.com/nution101/ttorch/commit/6b918a48ac8b73ecf09dbbcfe83e5016355564b4))
* **orchestrator:** cover the peer control channel's wiring ([68282a7](https://github.com/nution101/ttorch/commit/68282a7a150b641a6f94190e700d86b7da257a68))
* **orchestrator:** draw worker directories from the clone pool behind a flag ([c3f8fbf](https://github.com/nution101/ttorch/commit/c3f8fbf4933ca2460398b04cab15a4d3607192aa))
* **orchestrator:** launch a peer coordinator's manager under the peer charter ([eec258d](https://github.com/nution101/ttorch/commit/eec258dbf948ef8a9cbc573af99b223fa0124c16))
* **orchestrator:** let gate-change-approval off authorize a trusted gate change ([a2f7ac2](https://github.com/nution101/ttorch/commit/a2f7ac249f4434f7c5ac54558b3aceb57c90c4e7))
* **orchestrator:** report agent-exited when a worker's window outlives its agent ([4ad2c55](https://github.com/nution101/ttorch/commit/4ad2c55db6eb07ee24919223f9ca16be3649bb4a))
* **orchestrator:** start the scheduler and say what happened ([a3e66f4](https://github.com/nution101/ttorch/commit/a3e66f44131f8c315fe475b4c748f6bbf62618f3))
* **paths:** add a clones root beside the worktree pool ([f958131](https://github.com/nution101/ttorch/commit/f958131399d793ff7fd2875361daf6a034ec0041))
* **peer:** bind the parent to the control key's forced command ([fa7a3c0](https://github.com/nution101/ttorch/commit/fa7a3c0788c690d7b4094888f81c4b0d50dd3bb4))
* **peer:** build a versioned coordinator summary from existing reads ([a45e38b](https://github.com/nution101/ttorch/commit/a45e38bbc62307b9dea736cc15b0f0bf2527beb8))
* **peer:** build and install the authorized_keys line for a control key ([8fe2408](https://github.com/nution101/ttorch/commit/8fe2408d017a8986237ac3fe5a6668fdc8774cf0))
* **peer:** call a peer's control channel over ssh with only its control key ([9685e52](https://github.com/nution101/ttorch/commit/9685e520f9e996febdbba060c088d7b0a3a30b25))
* **peer:** remove other control keys when a peer is adopted ([ffb4b80](https://github.com/nution101/ttorch/commit/ffb4b8065e373e472e5ccaad19586abb8d1d1625))
* **peer:** report open escalations in the summary ([fc5dea8](https://github.com/nution101/ttorch/commit/fc5dea88d25a048fa21d08b48bb5cf57af174f9f))
* **peer:** serve the control verbs to a parent coordinator ([b04d989](https://github.com/nution101/ttorch/commit/b04d989aca22bf6598691026bca6c5fc4d763c9f))
* **peer:** take state-changing requests only from the parent ([db4f570](https://github.com/nution101/ttorch/commit/db4f570f0c955acb82806d905b4fc8aed3a709f5))
* **proc:** read process fingerprints on macOS and Linux ([338d3ec](https://github.com/nution101/ttorch/commit/338d3ec43882ca4ca5c38d6fced165e58c3fed36))
* **proc:** refuse to start a command whose timeout cannot be enforced ([2cd8d71](https://github.com/nution101/ttorch/commit/2cd8d71acb7b5a22eb6bd8a77d4b8e37d8e4be16))
* **proc:** start children so a deadline actually ends them ([e56cb18](https://github.com/nution101/ttorch/commit/e56cb18540aa1bafdc495054b44ab314d9517350))
* **projectinit:** keep the gate-change-approval line when init rewrites the block ([ea59cf4](https://github.com/nution101/ttorch/commit/ea59cf4419b0d0317cc92a65134ce6365b5d883d))
* **projectinit:** read a gate-change-approval policy line from AGENTS.md ([14ba3f2](https://github.com/nution101/ttorch/commit/14ba3f222de6c9b900c3f853ba789742f074b878))
* **project:** record the default branch at registration and seed existing projects ([6c4a8a5](https://github.com/nution101/ttorch/commit/6c4a8a5b5fc39999754f42361021f6e10e7cc731))
* **scheduler:** activate manager-window API-stall recovery ([c4516c0](https://github.com/nution101/ttorch/commit/c4516c0b7cbe437a278ce647009970e4f6916197))
* **scheduler:** add a load-aware dispatch backpressure governor ([436530c](https://github.com/nution101/ttorch/commit/436530c1f79fd226ca6ebbe77e85358f30922817))
* **scheduler:** add opt-in daemon gate-pass to take the manager off the steady-state land path ([d15cf2f](https://github.com/nution101/ttorch/commit/d15cf2f4c4b6ec2a72f5eecb78a4573d27f40ab0))
* **scheduler:** auto-gate trusted done-work in the autostarted daemon ([bf44830](https://github.com/nution101/ttorch/commit/bf448305ec4ec9d0ab282496442ee9d9272a1686))
* **scheduler:** auto-nudge alive-but-idle workers in the supervise pass ([0722229](https://github.com/nution101/ttorch/commit/07222295348160523d30de099ea71b07099d7b38))
* **scheduler:** auto-recover API-stalled sessions (worker live; manager pending invariant) ([085f29d](https://github.com/nution101/ttorch/commit/085f29dc0c166de69506924a0e6bc462f5c2d5a1))
* **scheduler:** count H4 governor deferrals in the scheduler-status deferred counter ([19fcf3a](https://github.com/nution101/ttorch/commit/19fcf3a51590b980bd2f6b718d1483f0553de9be))
* **scheduler:** dispatch file-overlapping tasks in parallel; surface land-rebase conflicts ([f2fe980](https://github.com/nution101/ttorch/commit/f2fe9803f3ef930e12a8ca2f7d9ce8051c7b8383))
* **scheduler:** export the serialize-overlap setting ([a835b61](https://github.com/nution101/ttorch/commit/a835b61657fce9c9d3e386ebf90f0a13ca9cc0cb))
* **scheduler:** make a stalled or idle daemon observable (heartbeat + status row + status cmd) ([db8253f](https://github.com/nution101/ttorch/commit/db8253f981cb1884ae4103bc995df6348f2be5dd))
* **scheduler:** poll registered peers on their own cadence ([7be9cc9](https://github.com/nution101/ttorch/commit/7be9cc99783824787b543a678da1bd629c17e715))
* **scheduler:** run the watch loop in the auto-started daemon ([9c77b4d](https://github.com/nution101/ttorch/commit/9c77b4d8d325dfeb7ffc59a703a6506957b28bbf))
* **skills:** bundle the ponytail agent skill, minimal-code worker default ([32ed80f](https://github.com/nution101/ttorch/commit/32ed80fc75ec009c66fa5dd36b3eb81c749d3e3f))
* **skills:** drop the recommendation published under the author's account ([0b100a8](https://github.com/nution101/ttorch/commit/0b100a8d5b5ca0931f255ce46780a4adc8b7b4b3))
* **status:** read the hook record in ttorch status ([6a0ce57](https://github.com/nution101/ttorch/commit/6a0ce573f090b95a3d1b6587808c503eebb4f476))
* **tmux:** add PanePIDErr, a pane pid read that reports its failure ([4e2d016](https://github.com/nution101/ttorch/commit/4e2d016535912c742d802fa31d0e1bae218bcbe5))
* **validate:** retry a severed-transport failure instead of failing the gate ([044672b](https://github.com/nution101/ttorch/commit/044672b748f7b6f5bef737934a9f9f91fbdee09d))
* **watch:** add an always-on watch loop that wakes an idle manager ([d787fee](https://github.com/nution101/ttorch/commit/d787feec232ed93a288eca4ac1fbfac86e2aae63))
* **watch:** add ttorch inbox, the one command a woken manager runs ([07649a4](https://github.com/nution101/ttorch/commit/07649a4684972a36b3a4a4fb5e0cc2534cd32a97))
* **watch:** keep raising a worker that sits silent at an idle prompt ([c899f1d](https://github.com/nution101/ttorch/commit/c899f1d946e0334bb8902b6d71e7a38e887a40f1))
* **watch:** log and show when the watch loop stands by ([1fa4d46](https://github.com/nution101/ttorch/commit/1fa4d46c52fa2fad3f82c239ae8a76c36fd4cf7e))
* **watch:** print the lead's answers in their own inbox block ([0acf820](https://github.com/nution101/ttorch/commit/0acf820276bbb222bd8c00b6d273bcb0e7d0d3f3))
* **watch:** print the parent coordinator's goals and answers in their own block ([dcefd55](https://github.com/nution101/ttorch/commit/dcefd55bcc879aea425b75b07668256cd3c7ac26))
* **watch:** read the hook record in the liveness sweep ([7682b6e](https://github.com/nution101/ttorch/commit/7682b6edf6c2d521b8b87aa8fbac6e7ed510c33f))
* **watch:** surface agent_exited when a live window has lost its agent ([99f64f3](https://github.com/nution101/ttorch/commit/99f64f3ae184d209400782b1b58e1b0de39ebd8a))
* **worktree:** drop a task's clone refs with DropImports ([89f5b4d](https://github.com/nution101/ttorch/commit/89f5b4dfc3ac011f6bf207afc66a428dbfc12467))
* **worktree:** import a clone's commit into main with ImportCommit ([844b0d1](https://github.com/nution101/ttorch/commit/844b0d19f549c942fa57dee97a9afeb80b4f5604))
* **worktree:** tell a clone from a linked worktree with KindOf ([dcf1551](https://github.com/nution101/ttorch/commit/dcf1551a11813fbd5cbff941f2aff881a1004440))
* **worktree:** validate task ids and object ids for the clone ref namespace ([f8cd443](https://github.com/nution101/ttorch/commit/f8cd4436ffe66c1772c0f6c529424954639b5c12))


### Bug Fixes

* **approve:** fail closed when the interactive test cannot evaluate ([c808b92](https://github.com/nution101/ttorch/commit/c808b92abd5d720c32ce965ea86acae1ee4c55fc))
* **board:** answer each question once across boards on one DB ([e358473](https://github.com/nution101/ttorch/commit/e358473b4d459ecfc97a20088b8c8e2a22cb31cb))
* **board:** cap the request method written by a refusal ([d402af8](https://github.com/nution101/ttorch/commit/d402af810e59ab141f1140cad4ef464431053d47))
* **board:** cap the request path written by a refusal ([ea1d412](https://github.com/nution101/ttorch/commit/ea1d4122757890b6608941ac135d0b8f739c4220))
* **board:** force overlap on a board dispatch only for a real conflict ([68342ca](https://github.com/nution101/ttorch/commit/68342ca8e212cd0755d9da2f58ce30983726ce51))
* **board:** keep = and a leading - out of the bare approval id ([8243454](https://github.com/nution101/ttorch/commit/824345439bebb48cc8735be09ce0d59e467db4de))
* **board:** log request-derived text quoted on one line ([078f449](https://github.com/nution101/ttorch/commit/078f449e371364b19ba9c999c185b358fdd789f5))
* **board:** refuse to answer an ad-hoc cc session ([78c2ba0](https://github.com/nution101/ttorch/commit/78c2ba0ab6b502a6225ddefbb80d77aab2b851f9))
* **board:** shell-quote the task id in the approval command ([93edbf1](https://github.com/nution101/ttorch/commit/93edbf18ea343826d94ce5a43535dab3c4671650))
* **brieflint:** a run that checked nothing is not a pass ([5194be1](https://github.com/nution101/ttorch/commit/5194be1389d4c673fb168a4de2dd0a1c8b751637))
* **brieflint:** answer "which sentence" by search, and check the budget where the work is ([f571061](https://github.com/nution101/ttorch/commit/f5710616e9ac3e35cc88dfdfcf6d5e0abfabc133))
* **brieflint:** bound a prohibition per clause, and stop claiming more than that ([cc1f1c4](https://github.com/nution101/ttorch/commit/cc1f1c4f4ed2ebe4c31df648cc1f6767dbeb77fb))
* **brieflint:** bound and fail closed the git work a brief can cause ([67e3944](https://github.com/nution101/ttorch/commit/67e3944c5a403abd0922dd3a6c5e08a2a8153be5))
* **brieflint:** cap how many findings one brief can produce ([5670075](https://github.com/nution101/ttorch/commit/567007536627a5d83a09b27ebc5a0323545af8aa))
* **brieflint:** cap the config file, and report a refused one as unevaluable ([d847017](https://github.com/nution101/ttorch/commit/d847017376395e67e13240f8bb5a3a6d686e8ec9))
* **brieflint:** check the run budget inside rule 5's loop, not once before it ([73ef841](https://github.com/nution101/ttorch/commit/73ef841813aec5d54fc6475d5c3e650e17b5cbc7))
* **brieflint:** close the two lows that were mis-stating what ran ([f4971ec](https://github.com/nution101/ttorch/commit/f4971ecb8ef0c2213e0decb2579920fb9742c154))
* **brieflint:** compute sentence boundaries once, not once per item found ([a8ac3d8](https://github.com/nution101/ttorch/commit/a8ac3d86bc4e5f9bdc521656e2c1daac7948e2e1))
* **brieflint:** count and quote hard counts per sentence, not per span ([037dbba](https://github.com/nution101/ttorch/commit/037dbba4d29d6203db75fe702c1674259776fec7))
* **brieflint:** count only the rules that actually ran ([82eaa05](https://github.com/nution101/ttorch/commit/82eaa051a3329321da7a0401724416b7522052e0))
* **brieflint:** earn the create exemption per mention, attach the hedge ([979c08f](https://github.com/nution101/ttorch/commit/979c08f6868a041d44a7b8704b8c6f333bda88c3))
* **brieflint:** enforce the run budget instead of declaring it ([b43dbe1](https://github.com/nution101/ttorch/commit/b43dbe16b27aed96aa15001d0f582d583ba12a17))
* **brieflint:** guard the ls-tree ref, and assert guard position ([743a970](https://github.com/nution101/ttorch/commit/743a97040c5551c8f7ed9313f6c8896b6c663d5f))
* **brieflint:** make the text phase linear and bounded on untrusted input ([b121cdf](https://github.com/nution101/ttorch/commit/b121cdfa0fd0a4c468c673c9dac7f32cd2b33c16))
* **brieflint:** narrow every within-sentence scope to the sentence, not the span ([403fcc1](https://github.com/nution101/ttorch/commit/403fcc10d3c95098becdbc2848c904371f7f260d))
* **brieflint:** quote config values on their way to the terminal ([9e7637c](https://github.com/nution101/ttorch/commit/9e7637c84ccb4a5601c5df6951a33eca0af19782))
* **brieflint:** quote git's stderr on its way to the terminal ([7d54f9b](https://github.com/nution101/ttorch/commit/7d54f9b920ce602c17e01d11cd6b2df02eba92b6))
* **brieflint:** report what the hard-counts rule checks, not what it intends ([3f2707b](https://github.com/nution101/ttorch/commit/3f2707bdd738ba49571360bad28978f1c83de117))
* **brieflint:** resolve a line citation against the base by default ([6d0e13d](https://github.com/nution101/ttorch/commit/6d0e13d7d4a84b82c0dc09af812abb83e3e730e1))
* **brieflint:** say what a size refusal actually refused ([294d607](https://github.com/nution101/ttorch/commit/294d607fd42d849065a942437f835782b85a6ba5))
* **brieflint:** say where a bound may sit, since the rule accepts two places ([9dc5404](https://github.com/nution101/ttorch/commit/9dc5404ce0db2fc317cba714f1e00a5c9a35f28e))
* **brieflint:** say which reason kept a rule from running ([ac14cf1](https://github.com/nution101/ttorch/commit/ac14cf171c00146766c898e1aee51981c46a95b7))
* **brieflint:** split sentences the way briefs are actually written ([f7b796d](https://github.com/nution101/ttorch/commit/f7b796d382b615cd133d4b27236cb530f124816b))
* **brieflint:** stop the create exemption falling to passive voice ([cd7b498](https://github.com/nution101/ttorch/commit/cd7b4981d31b9e6a47e7e4ae0da73d18e23842ee))
* **brieflint:** stop the splitter merging two sentences into one ([c59da18](https://github.com/nution101/ttorch/commit/c59da187aee338ba775b24e4165d46ddcb5c7f54))
* **brieflint:** truncate the list of unchecked targets ([843aec7](https://github.com/nution101/ttorch/commit/843aec7530b2ac20620f10e4b2e7c3dd83f89cf9))
* **cli:** give the peer control channel an environment of its own ([8403691](https://github.com/nution101/ttorch/commit/8403691a6ae6fe7d263887bf784f3d2ff7ca0fb7))
* **cli:** hand a new peer only the lead's model and effort settings ([da7a75a](https://github.com/nution101/ttorch/commit/da7a75a88ab8658d5e6817fc1da6daf529ff9501))
* **cli:** harden ttorch sync's git against the clone's config ([ce7ecbb](https://github.com/nution101/ttorch/commit/ce7ecbb395244f600987521a795e023b0797a9fd))
* **cli:** keep every path override out of peer.env ([d5b0543](https://github.com/nution101/ttorch/commit/d5b054325d11b69152f6a0544a49f61298747bf6))
* **cli:** keep the lead's repository path out of ttorch sync's output ([9e4005f](https://github.com/nution101/ttorch/commit/9e4005fad88d0b5521cb25cc3891f2b179fb8828))
* **cli:** leave directories others can write out of a new peer.env's PATH ([f0021a8](https://github.com/nution101/ttorch/commit/f0021a84d1af066691d1f822f18075af27341f04))
* **cli:** let only this package's errors choose an exit status ([cd87cac](https://github.com/nution101/ttorch/commit/cd87cac9eaf34c711174d73d28ebe655ed38d36d))
* **cli:** read every brief file under the cap, and refuse an oversize one ([d147157](https://github.com/nution101/ttorch/commit/d14715727bd2ed0c9e0bd824304dd444223ba69d))
* **cli:** record nothing from a hook in a review workspace ([2ad20b0](https://github.com/nution101/ttorch/commit/2ad20b0c22d92404ece76adad2c911a5429a1f03))
* **cli:** refuse a peer.env or ttorch home another account could write ([82ba738](https://github.com/nution101/ttorch/commit/82ba738fa32f9a77b30a397c90d0b6843bf529db))
* **cli:** refuse a worker context on every peer command that reaches a peer ([ebd0344](https://github.com/nution101/ttorch/commit/ebd0344035fa2e4e5c902bbaaea6d548b3c88125))
* **cli:** refuse follow-on and task ids that are not one plain token ([eb8134f](https://github.com/nution101/ttorch/commit/eb8134f67b772c0af42439d6c11db5f2922d26e9))
* **cli:** refuse inbox, watch and await-lead from a worker context ([390da1e](https://github.com/nution101/ttorch/commit/390da1e8883feb08149cdd684432a2f545b88e49))
* **cli:** refuse the peer client commands on a coordinator that is a peer ([6a0565f](https://github.com/nution101/ttorch/commit/6a0565f95377782813bbf1ac3c4ca4fc09d84e5d))
* **cli:** show the enforced delivery mode in `project ls`, sync it on init ([0ee85fd](https://github.com/nution101/ttorch/commit/0ee85fd234e89e84c227f36f6afd257da2a9014d))
* **cli:** write a task's brief in the transaction that adds it ([632ae71](https://github.com/nution101/ttorch/commit/632ae71b311ed465f9d683fad65371a6df24e653))
* **content:** unexport the embedded payload behind a read accessor ([7bc7754](https://github.com/nution101/ttorch/commit/7bc775464942a30adaf39e7800463f8df7a88682))
* **db:** start a peer's poll state over when it is registered again ([daf9634](https://github.com/nution101/ttorch/commit/daf9634bb8ce7f0a84cc52003cda037821b19ec6))
* **doctor:** check the clone git floor with the import's own check ([1d09fa3](https://github.com/nution101/ttorch/commit/1d09fa3be85ade74f61fed8ecffc924b6856e867))
* **doctor:** raise the per-worker clone git floor to 2.45.1 ([806d225](https://github.com/nution101/ttorch/commit/806d225ea6641aeecca33395c243d7a7da6737a7))
* **gate:** --no-renames on diffLineStat too ([6866803](https://github.com/nution101/ttorch/commit/68668036e1426ecdd523e75eb32e5f16411b94a7))
* **gate:** ask git for both sides of a rename ([b03feaa](https://github.com/nution101/ttorch/commit/b03feaa9b60ceb6e79c23dbfda592ba6159e04b4))
* **gate:** block when the reports directory cannot be listed ([52cec36](https://github.com/nution101/ttorch/commit/52cec36ebe074f42ff198c57bf456501bc286e30))
* **gate:** bound a reviewer launch that keeps failing ([2bb1930](https://github.com/nution101/ttorch/commit/2bb1930a371c2a5f094f4abb0cdc2c068f311bdc))
* **gate:** bound the episode where every unfinished route passes ([9957ef9](https://github.com/nution101/ttorch/commit/9957ef9950363210eba493e101aec8372544ff4d))
* **gate:** carry a blocking head-pinned report across a re-prep ([8c2b564](https://github.com/nution101/ttorch/commit/8c2b5642296ab6385d605a5da679ac109038c832))
* **gate:** case-fold the gate-config match and unquote its input ([2ce0a3f](https://github.com/nution101/ttorch/commit/2ce0a3fe809bd42c99437b46dadd610b03611025))
* **gate:** charge a reviewer launch by failure kind, not uniformly ([a21345c](https://github.com/nution101/ttorch/commit/a21345c7b696b7500eafa5f87993dffa3279fc42))
* **gate:** compare paths the way the filesystem does, and refuse collisions outright ([c858102](https://github.com/nution101/ttorch/commit/c8581022234aae0936a1e43a295351f73138b1a2))
* **gate:** cover the evidence layer, and derive what counts as a proof ([16ab700](https://github.com/nution101/ttorch/commit/16ab7007c9cc61bfc9513c4bd7da5e635c756b60))
* **gate:** cover the gate's own proofs, and stop testing them vacuously ([c44b418](https://github.com/nution101/ttorch/commit/c44b418e8f0e0cf12e2fd1e6cf301effb823c5b8))
* **gate:** cross-check the stall clock against what a worker cannot write ([4a877dd](https://github.com/nution101/ttorch/commit/4a877ddcdb92c68356102d1c6f5f34a231c5c46a))
* **gate:** derive the required review dimensions as a floor at record time ([080371e](https://github.com/nution101/ttorch/commit/080371ef59a79a6b7d3d2487f7496d99d1d55050))
* **gate:** derive the reviewer floor over every candidate base ([083b66e](https://github.com/nution101/ttorch/commit/083b66e1d39e35678ca0e4959c6e2d09fcfa14b4))
* **gate:** diff the reviewers' and the land's base by fully qualified ref ([1a58ca2](https://github.com/nution101/ttorch/commit/1a58ca27e8e4481f09e219312ca281fbb5be8b34))
* **gate:** diff trust-prep review against the branch's true base, not stale local main ([a57ad51](https://github.com/nution101/ttorch/commit/a57ad5185ae7b88c7f903234f1584d86ee2d6364))
* **gate:** drop git from the scope test so it runs in the gate lane ([b6c686e](https://github.com/nution101/ttorch/commit/b6c686edabd84240365f426fa858e61dbaa6fcae))
* **gate:** enumerate symlinks without git so the check runs in the gate lane ([5c74198](https://github.com/nution101/ttorch/commit/5c741987452218fba12789a0d0dfcb0ac29d1609))
* **gate:** fail closed when the episode's dispatch record is lost ([8875416](https://github.com/nution101/ttorch/commit/88754164878ce2aa7c4841ba89d6f23ed0986ee0))
* **gate:** fold every report pinned to head, not the ones the record lists ([83c9abf](https://github.com/nution101/ttorch/commit/83c9abf5a5e57b6f8e8b0dde7858ee671052d3e9))
* **gate:** fold Unicode, refuse colliding paths, stop forging audit records ([c8c4c92](https://github.com/nution101/ttorch/commit/c8c4c922ccce106b93384175f1b3bd122ada25e0))
* **gate:** generate the cost figures, and refuse a hand-written one anywhere ([ca2822d](https://github.com/nution101/ttorch/commit/ca2822da5ee3b4c80ee0b78098716bb2e9b1e903))
* **gate:** give the advisory audits their own review episode ([73c78c0](https://github.com/nution101/ttorch/commit/73c78c05a1f316bcf7f5691619bb610b2797f834))
* **gate:** keep a dispatched dimension required until it reports ([590c30b](https://github.com/nution101/ttorch/commit/590c30bb9feb7ea80b9720fd78ad97c325efb40c))
* **gate:** keep only the decision shape in the authority validate memo ([9f50a36](https://github.com/nution101/ttorch/commit/9f50a368200a8e4b09d81e81d13e7122702fe210))
* **gate:** keep the advisory guard up when the episode row is deleted ([76b12f3](https://github.com/nution101/ttorch/commit/76b12f37c74a23ec4fa38cbdefc2a7b4d782fc0f))
* **gate:** leave the default-branch seed and its notice to the lead ([0db97ce](https://github.com/nution101/ttorch/commit/0db97cedc551818fffdf704611a5ac5e11cc23f3))
* **gate:** match a covered directory's own path, and refuse links over it ([c1fef23](https://github.com/nution101/ttorch/commit/c1fef23ed6a2b61ac3d852ad492b2ea9443898f8))
* **gate:** measure every published cost figure, not three table rows ([c300085](https://github.com/nution101/ttorch/commit/c300085528e54da23f2bcc4560465f1d74f797d7))
* **gate:** move skipIfShort into a covered file ([2519783](https://github.com/nution101/ttorch/commit/2519783c2fb1959fe8604a22eed554180a78682f))
* **gate:** move the episode record into the store ([2f4f01e](https://github.com/nution101/ttorch/commit/2f4f01e87c95280b9fcfcd01fb3ae36a934fff37))
* **gate:** move the reviewer's scratch workspace out of the review-inputs dir ([6b2a94f](https://github.com/nution101/ttorch/commit/6b2a94fcbb219569b2040827ef71801d4ea0f68b))
* **gate:** never discard a report from a reviewer the gate dispatched ([6cf84b9](https://github.com/nution101/ttorch/commit/6cf84b9daac8664c7d5fc9d0470bd339c5565a69))
* **gate:** pin the verdict to the base its review diff started from ([cc6bb7f](https://github.com/nution101/ttorch/commit/cc6bb7fef7a8a1b3e77db22ec1aafef4e2f727b4))
* **gate:** reach the covered-path lists, and sweep the proofs the symbols miss ([d4793c0](https://github.com/nution101/ttorch/commit/d4793c04e2b1f77894098f8b27d4dbfb799c3d35))
* **gate:** read the default branch by fully qualified ref, once per gate run ([7d19b32](https://github.com/nution101/ttorch/commit/7d19b32261ebdd2ea277387615b441795a4d4437))
* **gate:** read the gate base from the recorded default branch only ([d8ca650](https://github.com/nution101/ttorch/commit/d8ca650e8663619e55c480594ec8d0dad248376d))
* **gate:** read the scope signal from the raw blob, not from git grep ([74da5d4](https://github.com/nution101/ttorch/commit/74da5d46226368bcb7adde4d5c1edc29fcd33e46))
* **gate:** refuse a grant path that the token parser would split ([56c23d1](https://github.com/nution101/ttorch/commit/56c23d11e3da4295f113fa7d5422934bb5ae3281))
* **gate:** refuse a review workspace whose ancestors carry session config ([ec60f14](https://github.com/nution101/ttorch/commit/ec60f1415f653bdd789553a265a99bc72abcb001))
* **gate:** refuse a review workspace whose mirror lacks the reviewed commit ([700f914](https://github.com/nution101/ttorch/commit/700f914cd8248ee4c5ae590e2359fe6041e9b3c9))
* **gate:** require the green that authorizes a merge to be a run this process made ([076a44f](https://github.com/nution101/ttorch/commit/076a44fa3533493277eb226477616a5f372ff61b))
* **gate:** return the error when an episode write fails ([750da3f](https://github.com/nution101/ttorch/commit/750da3f4ae999c14fb87eb7f8bdd279dfb5f9a46))
* **gate:** run every reviewer outside the worker's worktree ([5862769](https://github.com/nution101/ttorch/commit/5862769b9ff998dbad9bae9f925a278aa9b02f4b))
* **gate:** run the security reviewer outside the worker's worktree ([e358bf4](https://github.com/nution101/ttorch/commit/e358bf4045ed3fb13d30aa858db74df2bf23d413))
* **gate:** run the stall clock from the episode, not the first dispatch ([e6e5b4a](https://github.com/nution101/ttorch/commit/e6e5b4a222ecbfaf32333c594c1f416a2c6e944e))
* **gate:** scope the covered set to the repository it is gating ([3993158](https://github.com/nution101/ttorch/commit/39931580dda3d663404f47dea4985b9dc91dc33f))
* **gate:** stop the advisory guard resting on a worker-writable file ([9482cbf](https://github.com/nution101/ttorch/commit/9482cbfe75e8df3a723bf70c0c9c3574ed7bacd5))
* **gate:** stop the advisory prep resetting a live gate episode ([0a30cd1](https://github.com/nution101/ttorch/commit/0a30cd13ecbe79ba7effa8759782a2e8d21a84e8))
* **gate:** stop the land pass staging a reused green over prep's validate ([d741aaf](https://github.com/nution101/ttorch/commit/d741aafe292f60c79d581e25e7b6ccde303988c9))
* **gate:** stop the reviewer brief presenting validate.json as proof ([d7ba85d](https://github.com/nution101/ttorch/commit/d7ba85dff13637b974975e961837516cd6b76a04))
* **gate:** validate the task id before it becomes a delete path ([6548ba1](https://github.com/nution101/ttorch/commit/6548ba184c1fd787a5fe67ff5ac33aed438cf18a))
* **gate:** validate the workspace dimension with the shared predicate ([d4c3b34](https://github.com/nution101/ttorch/commit/d4c3b345ab42d408a859232756e0574219b8dea5))
* **gate:** walk the reviewer workspace's resolved path, not its logical one ([01ffc1d](https://github.com/nution101/ttorch/commit/01ffc1dfccdab2ce64758e04e63114c9cc2bbb9d))
* **gate:** write the reviewer's prompt 0600 into a 0700 workspace ([466f4eb](https://github.com/nution101/ttorch/commit/466f4ebc669d7eabebce64a446ab28fd55d41d89))
* **gate:** write the reviewer's prompt where the worker cannot ([35568d1](https://github.com/nution101/ttorch/commit/35568d1706f119beb0d32838e13967d5ab2cab17))
* **harness:** give clone workers a private system git config ([5d84adb](https://github.com/nution101/ttorch/commit/5d84adb5dbb61e5707c2fc2ce81f3f4c47e2d2db))
* **harness:** refuse a clone slot reached through a symlink ([b99ef96](https://github.com/nution101/ttorch/commit/b99ef96f4e6ddb069f62cfce18f3681c65f6ce9b))
* **harness:** rewrite a clone slot's private git config for every harness ([a8df3ee](https://github.com/nution101/ttorch/commit/a8df3ee5fa65aeedd7be465bab39058d999740c7))
* **harness:** say a parent's answer is as unverified as its goal ([707da39](https://github.com/nution101/ttorch/commit/707da39ae12ee806ea91af9a5d12cf50bffce5ad))
* **herdr:** match ErrWaitTimeout only for agent.wait's timeout ([f3098e3](https://github.com/nution101/ttorch/commit/f3098e336834567a11c15e16d08dbf893197e236))
* **herdr:** quote server-supplied text in error strings ([bd662d3](https://github.com/nution101/ttorch/commit/bd662d3de29015a495d8a810b06545aecad2f95d))
* **herdr:** refuse a server not run by the current user after connecting ([696abd1](https://github.com/nution101/ttorch/commit/696abd1cf4f9ce0ec19bb4fc13e8276f0be0f062))
* **herdr:** refuse a socket another local user could have planted ([c154599](https://github.com/nution101/ttorch/commit/c154599ed7c88f68e27ba96989ee08a30de89ed3))
* **herdr:** reject session names that could leave the sessions directory ([0ae5ded](https://github.com/nution101/ttorch/commit/0ae5ded6ef5b3012dd902019063eae19198b44bb))
* **hook:** write no hook record when identity sources disagree ([2ab0abf](https://github.com/nution101/ttorch/commit/2ab0abf271feb9be4e42d8742b37bec172500443))
* **installer:** choose the embedded payload inside the covered package ([12219b0](https://github.com/nution101/ttorch/commit/12219b0a138fda77ee0c78841544a46b08ba5047))
* **livestate:** refuse a hook record that is not a small regular file ([9f19f0c](https://github.com/nution101/ttorch/commit/9f19f0cc5fa6ce4d8fac26ca7f7303d581eb7362))
* **make:** refuse a shard count the shard loop cannot use ([b850358](https://github.com/nution101/ttorch/commit/b850358e190c628ef9790f82ceb1c76b60bc9b5a))
* **model:** never write code below opus; confine sonnet to research ([371b764](https://github.com/nution101/ttorch/commit/371b76403d8172d044f808d2eaf090f30a3c0ebc))
* **model:** never write code below opus; confine sonnet to research ([087b417](https://github.com/nution101/ttorch/commit/087b417685c288956484f24e5562ab0cc816ffdd))
* **orchestrator:** cover the peer client and both sides of provisioning ([e2e4ff6](https://github.com/nution101/ttorch/commit/e2e4ff6b7de7947f10579f4871640f33e06a4f38))
* **orchestrator:** cover the peer pass and its client adapter ([6d656b1](https://github.com/nution101/ttorch/commit/6d656b19e8f47c0b2a9ad101cfdb099952fc1eb8))
* **peer:** cap task ids and repo paths in the summary and decisions ([bd3b33c](https://github.com/nution101/ttorch/commit/bd3b33c7d3ea59b18c94a71a2b0ccfcf93171e52))
* **peer:** check a private directory through one descriptor ([2459f1f](https://github.com/nution101/ttorch/commit/2459f1f13017d710aaf6618e8d46810558fa1481))
* **peer:** check every directory above the forced command's binary ([e8173a4](https://github.com/nution101/ttorch/commit/e8173a4026f3e1d98736872aca370aeccaee80fa))
* **peer:** check the parent before ensure-up restarts a peer's fleet ([99c2206](https://github.com/nution101/ttorch/commit/99c2206876eae9c5a5d5738e361884fd569690a0))
* **peer:** refuse to make a coordinator with peers of its own a peer ([0cf1a6a](https://github.com/nution101/ttorch/commit/0cf1a6ac2f976163ab84e89ec696a701174784a9))
* **peer:** serialize control key installs on the ssh directory ([4f0057e](https://github.com/nution101/ttorch/commit/4f0057e897f59e51d8f8ed68c47374f6fd60376b))
* **peer:** set a new private directory's mode through its descriptor ([4c56ce5](https://github.com/nution101/ttorch/commit/4c56ce583658e8535062144ea8c50fad0b3a06ee))
* **proc:** bind the group kill to the command it was installed on ([c15e1b9](https://github.com/nution101/ttorch/commit/c15e1b9af9d131c86a2a515a6f5074185318cb9e))
* **proc:** keep an agent alive across an in-place exec ([cc267ee](https://github.com/nution101/ttorch/commit/cc267eef81938d83f6bdbe910af5482b6c2caf20))
* **proc:** read the Cancel binding instead of firing it, and stop discarding findings ([fae0556](https://github.com/nution101/ttorch/commit/fae0556d4cf7640a8fa7c39c0a01b6d77a659d0a))
* **proc:** run ps from /bin/ps instead of looking it up on PATH ([c87ef11](https://github.com/nution101/ttorch/commit/c87ef1132c3ce0e48baa9c31ba30483cfb06181e))
* **proc:** stop a FIFO at the fingerprint path blocking every sweep ([976419e](https://github.com/nution101/ttorch/commit/976419e45726a317d83327cedab706c6d3c616b4))
* **projectinit:** treat a malformed gate-change-approval line as required ([bab5a4f](https://github.com/nution101/ttorch/commit/bab5a4f62e44ad5351b376ada64a5b947f016004))
* **project:** keep a registration's notice when the seed loses the race ([adc02c8](https://github.com/nution101/ttorch/commit/adc02c8c10632878d1f823f06fd5799e1e641d47))
* **project:** record the branch from project add and init only for the lead ([c5fd8d3](https://github.com/nution101/ttorch/commit/c5fd8d3a2183d3ad337ec2d715382aa22551075f))
* **review:** archive the dimensions the previous prep prepared ([bc06caf](https://github.com/nution101/ttorch/commit/bc06caf03a0d7017b16bd0af9745d6b68cf1aa8a))
* **review:** block a verdict over an empty dimension set ([46acd95](https://github.com/nution101/ttorch/commit/46acd95d77dc67bf263bf3ddd693cd9472e7ea26))
* **review:** block when an extra dimension's report cannot be read ([7eb8ee7](https://github.com/nution101/ttorch/commit/7eb8ee7c48f9113cc8b24f764eb4f515013e44fd))
* **review:** corroborate the prep marker against the staged validate ([bb9aaeb](https://github.com/nution101/ttorch/commit/bb9aaebe2b006cf93aae3d12a00cbba4d19b8874))
* **review:** cover every subtree the installer writes into a session ([6baa1d9](https://github.com/nution101/ttorch/commit/6baa1d9b60eb91f0e7c21b1ac79e9e4f48607fcd))
* **review:** fold the directory segment, not just the basename ([6841b36](https://github.com/nution101/ttorch/commit/6841b363751c25707c22e737c82e6152ee3010b4))
* **review:** guard the manual path and widen the sink scan ([b211df3](https://github.com/nution101/ttorch/commit/b211df3f58838be3b59f7cf3f846ecd02ab82bfe))
* **review:** keep reports in their own directory, away from the control files ([a0621d5](https://github.com/nution101/ttorch/commit/a0621d5e1f48d655f672ad89e868b70065ce8c8e))
* **review:** quote severity too, and strip the separators SafeLine missed ([f9afe2e](https://github.com/nution101/ttorch/commit/f9afe2ee94300584701f726f6a037efb7601e4d7))
* **review:** quote the filename the legacy sweep reports on ([cc08055](https://github.com/nution101/ttorch/commit/cc08055d87a3952a9f46060a3f00308cbd9c73f4))
* **review:** refuse a clean verdict when the staged validate is not green ([4a8cfec](https://github.com/nution101/ttorch/commit/4a8cfec5cd1678d5fce32b16b8f205ad57198b03))
* **review:** render reviewer free text as quoted data, not as output ([2aa4748](https://github.com/nution101/ttorch/commit/2aa4748307dab6189626c94f9a3ede09133ad282))
* **review:** spare the advisory verdict files by name, not by luck ([8113c37](https://github.com/nution101/ttorch/commit/8113c37010d40aeb46f0cf1f40369edbe765081e))
* **review:** stop agent instruction files classifying as inert prose ([cf88074](https://github.com/nution101/ttorch/commit/cf880749356fd2e4deb0ee508242fadd74d10b5b))
* **review:** take the required reviewer set from the prep stamp, not the file ([22746ee](https://github.com/nution101/ttorch/commit/22746eec3d1d738509703e0b76ee2deb93208ed1))
* **review:** treat a report from a superseded prep as no review at all ([8fcc5be](https://github.com/nution101/ttorch/commit/8fcc5be36dfc9bf19a991ca842a52bbc327344cf))
* **review:** treat agent configuration as code, not inert prose ([6e19dda](https://github.com/nution101/ttorch/commit/6e19ddae72aa2df6a8275d259d5b73d4b1e6fb6e))
* **review:** validate dimension names at every sink, not just the archive ([aef35c0](https://github.com/nution101/ttorch/commit/aef35c0853771d220ef296d9e472f09987d72aa6))
* **review:** validate dimension names before they become paths ([9077f82](https://github.com/nution101/ttorch/commit/9077f8229831cd60379471f0716836b7d180f3bc))
* **review:** validate the dimension set before composing the block payload ([9c32075](https://github.com/nution101/ttorch/commit/9c320755f75f06fb189e9d2de5680fe4e8831f8e))
* **scheduler:** bound ensure-up per peer per hour, not per down episode ([9b3dc95](https://github.com/nution101/ttorch/commit/9b3dc95dc691305c2ffb5548da68b80a1a8b9085))
* **scheduler:** bound how long one gate pass can hold the tick ([587ff86](https://github.com/nution101/ttorch/commit/587ff863d4e2cc7c6c62d5333dae050b4021576a))
* **scheduler:** classify dispatch failures; park permanent, back off transient ([bb9c989](https://github.com/nution101/ttorch/commit/bb9c989536ac21967c152e9e3e955065221369ad))
* **scheduler:** notice a peer whose escalation ids went back, and refuse ids out of range ([bdf5cfd](https://github.com/nution101/ttorch/commit/bdf5cfdbb6a7f7027ed9c0d1b36b4ee2586e7c9c))
* **scheduler:** require a stored brief before auto-dispatching a task ([a3ff7bc](https://github.com/nution101/ttorch/commit/a3ff7bc553453800bd817c74e7a75749d745cbbe))
* **scheduler:** route the manager stall nudge through the session backend ([d98f468](https://github.com/nution101/ttorch/commit/d98f468a5b62a0e4fa269fbd198e11db2d0f864c))
* **sync:** bound git network calls and skip the sync after a failed fetch ([09538f5](https://github.com/nution101/ttorch/commit/09538f5f106d3d439e44a691d9a5460a18d3da22))
* **sync:** fast-forward fleet-sync to the commit origin reports ([7da2140](https://github.com/nution101/ttorch/commit/7da2140d33a850b80f1a95cdd38db96987c99102))
* **teardown:** check unmerged work against fully qualified refs ([6438a55](https://github.com/nution101/ttorch/commit/6438a556e3ed6e85c65bf77942d9ca814fedced6))
* **termtab:** detect iTerm2 installed in ~/Applications ([b642ba6](https://github.com/nution101/ttorch/commit/b642ba676d5db9012e2ffda6c56be11880323b51))
* **termtab:** detect iTerm2 installed in ~/Applications ([91847a2](https://github.com/nution101/ttorch/commit/91847a2bc85c6018649a9bbefda348857644f54b))
* **termtab:** disclose the switch-client escape, and stop a wedged probe eating a retry ([e4b7f0a](https://github.com/nution101/ttorch/commit/e4b7f0a4658449cca73eaa0c0953169015be900c))
* **termtab:** make the worker view tab read-only ([48e0f30](https://github.com/nution101/ttorch/commit/48e0f3093f873a2b27c6e5503d2c71d24d989789))
* **test:** keep fixture git out of the repository an inherited GIT_DIR names ([85ee04c](https://github.com/nution101/ttorch/commit/85ee04c733ecc3bcf55610cd9a2581b43519a8ec))
* **test:** pin the private tmux socket so test cleanup cannot kill the default server ([f4c3e9e](https://github.com/nution101/ttorch/commit/f4c3e9ebf4d6fbc6fff59cab41339b0472f711c4))
* **tmux:** bound every tmux command, and stop the view gate failing open ([9cff456](https://github.com/nution101/ttorch/commit/9cff456bb0ab669d25f076a82229b3897f290be5))
* **tmux:** dedupe windows on create so recovery never stacks a duplicate ([740933c](https://github.com/nution101/ttorch/commit/740933cfc1f19c239794696851c760608bc08a96))
* **tmux:** make a timeout distinguishable, and bound the last tmux call ([0390e5e](https://github.com/nution101/ttorch/commit/0390e5ecb969d75955b62d0a340f1ef026d5739c))
* **validate:** say what the pre-start check actually verifies ([74b315a](https://github.com/nution101/ttorch/commit/74b315ae105680d48b6bb944039883eb48aa3828))
* **watch:** back a task's HEAD reads off after a slow read ([f80e600](https://github.com/nution101/ttorch/commit/f80e60097a5c6eeb1a816e80be8c558361e88567))
* **watch:** bound the stall ladder's HEAD read with a deadline ([d33042c](https://github.com/nution101/ttorch/commit/d33042cfcf4982bcfec001a3f9e3ad5cd7d2ef3b))
* **watch:** confine the HEAD read to the worktree and its admin entry ([22fd78f](https://github.com/nution101/ttorch/commit/22fd78f6bfdc690a383bc7fe245d6ce9fdac062c))
* **watch:** follow no symlink anywhere on the HEAD walk's paths ([6cc8a99](https://github.com/nution101/ttorch/commit/6cc8a99a3a8ece6928558dbb9e1300e3dadbaab2))
* **watch:** harden the singleton instance token against pane-pid reuse ([6399b4a](https://github.com/nution101/ttorch/commit/6399b4a8366af26148291cf5afb73537ba970e04))
* **watch:** make a refused singleton acquisition exit non-zero and say so ([36c3a96](https://github.com/nution101/ttorch/commit/36c3a963342cda1756a081fc305508b30e9f19bf))
* **watch:** mark the wake as automated and inbox text as worker data ([69368cb](https://github.com/nution101/ttorch/commit/69368cbd1641f79193c569922c217ffe0f1200f4))
* **watch:** only wake when the harness leads the manager pane's foreground ([14d5adf](https://github.com/nution101/ttorch/commit/14d5adf8739b35c36ef1c4c5f7e2c1fedac6f439))
* **watch:** press Enter on the wake only after the pane shows exactly it ([73067b6](https://github.com/nution101/ttorch/commit/73067b6914bfe80f944eb2366ef6fa315dedbd2e))
* **watch:** press Enter only from a function that checks the pane itself ([54f2268](https://github.com/nution101/ttorch/commit/54f2268bf148825a0f778204162a8190f7e05bd7))
* **watch:** print every peer event on its own, as peer text ([7aa7b86](https://github.com/nution101/ttorch/commit/7aa7b8664b2a165687fe14c2764243df543dd7fc))
* **watch:** print hand-armed watch batches in the inbox's worker block ([a1e5470](https://github.com/nution101/ttorch/commit/a1e5470eef62ca7196ca189fccb7c9a5b8c3dba9))
* **watch:** put a one-minute floor under the stall thresholds ([126baec](https://github.com/nution101/ttorch/commit/126baecdac908d812bdd5609d87ca2e9ac554c27))
* **watch:** read a reftable repository's HEAD as unknown ([76debdf](https://github.com/nution101/ttorch/commit/76debdf80b40a3edd77a6d88a6faee2593932c4a))
* **watch:** read a worker's commit time without running its git config ([7af7e40](https://github.com/nution101/ttorch/commit/7af7e40508a669bab0ef4411ede792b177e35105))
* **watch:** read a worker's HEAD from files instead of running git there ([bf0f17e](https://github.com/nution101/ttorch/commit/bf0f17e0e2e576e6724b7c39b933120ba4fb2eb3))
* **watch:** read the hook record in the stall ladder ([948a5e1](https://github.com/nution101/ttorch/commit/948a5e195dd7488a2a5fa2b221a93081e07b7e34))
* **watch:** read the wake's prompt from the input box, not the last caret ([d8bc882](https://github.com/nution101/ttorch/commit/d8bc882fd4893f969d5cef8e778de0f670855384))
* **watch:** run the idle check when the agent state is unknown ([54ed9ce](https://github.com/nution101/ttorch/commit/54ed9ce88627bdc56a06f39ed2ac8a7f666affe5))
* **watch:** stop agent_exited masking a task's later liveness events ([2bc5f7d](https://github.com/nution101/ttorch/commit/2bc5f7d36ce08100e3fb6ec2c704efe061e32009))
* **watch:** stop an armed watcher re-surfacing updates another consumer took ([a2e9ebf](https://github.com/nution101/ttorch/commit/a2e9ebfe25de8c7ab40b245c20c5819163be6945))
* **watch:** stop calling a relayed answer the lead's in the inbox ([2b53a42](https://github.com/nution101/ttorch/commit/2b53a4283e8262e91377956924ef3fbe359bd719))
* **worker:** enforce ttorch report via a worker Stop hook ([682beb8](https://github.com/nution101/ttorch/commit/682beb8f852baefccbdc343aedead405739d6731))
* **worker:** enforce ttorch report via a worker Stop hook ([9e090ee](https://github.com/nution101/ttorch/commit/9e090eea5a570417605e31155a0aae8326fadda3))
* **worktree:** frame the batch blob read so it cannot desync or fail open ([0c25adc](https://github.com/nution101/ttorch/commit/0c25adc60f4834db991a9a2f36be54c83342d831))
* **worktree:** never recycle a pool slot that still holds unlanded work ([fda599d](https://github.com/nution101/ttorch/commit/fda599dbf7f01913e488d2cffd1ed2717faa5889))
* **worktree:** qualify the task base and the slot reuse check ([e3c30d9](https://github.com/nution101/ttorch/commit/e3c30d9e6f57ada77e52d99109ce2689ac34b861))
* **worktree:** read base blobs by object id, not by path ([3aa2ca1](https://github.com/nution101/ttorch/commit/3aa2ca1017d44507ddab1bb695a13d9908f94a0c))
* **worktree:** refuse the import's lazy fetch and raise the git floor to the CVE-2024-32004 fix ([2b9d207](https://github.com/nution101/ttorch/commit/2b9d207b1e194a906cc53d8cf2f73285f60ab2ce))
* **worktree:** refuse to import on a git older than 2.32 ([493f249](https://github.com/nution101/ttorch/commit/493f249a362271ffeb1ec5d0e8408839c7c338cf))
* **worktree:** reject a malformed size, and check ls-tree like its sibling ([fbebbd6](https://github.com/nution101/ttorch/commit/fbebbd60086bdffd9078d854c4956afa150e02fc))
* **worktree:** run the import without the caller's GIT_* environment ([da0492f](https://github.com/nution101/ttorch/commit/da0492fb9563d7dc172c4bc5067d448e89ddf1d4))
* **worktree:** stop main's config and a bundle file from subverting the import ([885f17c](https://github.com/nution101/ttorch/commit/885f17c26ccc0f7980ca7e6650908fac304474fb))
* **worktree:** unblock validate below the git floor and close two more import config vectors ([bb5f761](https://github.com/nution101/ttorch/commit/bb5f761b051171fd0b58493a489e7e585c7f34d6))


### Performance Improvements

* **scheduler:** snapshot the live fleet once per dispatch tick ([fab69d8](https://github.com/nution101/ttorch/commit/fab69d8a1eac4bb92845e90f9f3b6cbcf469209e))

## [0.18.0](https://github.com/nution101/ttorch/compare/v0.17.0...v0.18.0) (2026-09-23)


### Features

* **approve:** refuse a non-interactive or worker-context approval; this narrows rather than closes the route, since a same-uid process can bypass it, and an approve run through a script or wrapper now fails ([021773e](https://github.com/nution101/ttorch/commit/021773e16174dbf0ea5568c5b03bf65626a8d1b6))
* **brieflint:** add --offline, so an unreachable remote costs one rule ([57f91c1](https://github.com/nution101/ttorch/commit/57f91c175965f2b42c68e825f4c95000d443204b))
* **brieflint:** check a task brief before it is stored ([fd35159](https://github.com/nution101/ttorch/commit/fd351598055f1617f54c48b298a1c45910471627))
* **brieflint:** give a reduced-coverage run its own exit status ([09699cf](https://github.com/nution101/ttorch/commit/09699cfc92c3a9f5ad1e87085a62d65adea38547))
* **cli:** add ttorch brief-lint ([eb2cb5f](https://github.com/nution101/ttorch/commit/eb2cb5f08c4ffc30e22fcb4cd3b153d35625d42e))
* **cli:** lint a supplied brief at task add ([0a1dbcd](https://github.com/nution101/ttorch/commit/0a1dbcd5dfd35e0a4b2af18bc113aaece8b59915))
* **cli:** lint the brief spawn stores, as task add already did ([5878ae2](https://github.com/nution101/ttorch/commit/5878ae26dee38ce96e45ff484417e3c1dc70056d))
* **gate:** bind a gate-change approval to the files it was granted for ([f149d1d](https://github.com/nution101/ttorch/commit/f149d1d0b4adf4b4b0f8edb98f221f12c63f9975))
* **gate:** cover CLAUDE.md, the symlink the guard matched by its own path ([9ad93f8](https://github.com/nution101/ttorch/commit/9ad93f88ea318fcff38259f16a7d32ed403e5378))
* **gate:** cover go.work, go.work.sum and vendor/ ([de19c56](https://github.com/nution101/ttorch/commit/de19c5684c29ca5dc70f6efd50a56c6c610f9d8c))
* **gate:** cover internal/skills/, the shortest route into ~/.claude/skills ([265e0e2](https://github.com/nution101/ttorch/commit/265e0e2ab7c7651c9e683a0257c5cb309b834895))
* **gate:** cover project agent config, go.mod/go.sum and nested instruction files ([84e749b](https://github.com/nution101/ttorch/commit/84e749b6feadcdf0ea46a0ac31d8d8b83b50996a))
* **gate:** cover the deciding code and CI in the gate-config guard ([d6896b2](https://github.com/nution101/ttorch/commit/d6896b2366c2f7948ef47edf5d6869fba1cb44fb))
* **gate:** cover the gate's own instructions in the gate-config guard ([9a2b8de](https://github.com/nution101/ttorch/commit/9a2b8dee82f2a39ddca30bd61b172007bf066ab6))
* **gate:** cover the whole content/ tree, not the subtrees the gate dispatches ([0c8baa6](https://github.com/nution101/ttorch/commit/0c8baa6c26891356ee56eeb049089996b0568b9c))
* **gate:** make .ttorch/ a prefix, closing the learnings write channel into AGENTS.md on gated merges (trusted mode or --require-verdict) ([95147d3](https://github.com/nution101/ttorch/commit/95147d3f8a4631360220191dc47a126150020fee))
* **gate:** require an explicit approval to merge a gate-definition change, on gated merges (trusted mode or --require-verdict) ([69309fc](https://github.com/nution101/ttorch/commit/69309fc761656ea3e9be890faf07d52c473c2525))
* **proc:** refuse to start a command whose timeout cannot be enforced ([2cd8d71](https://github.com/nution101/ttorch/commit/2cd8d71acb7b5a22eb6bd8a77d4b8e37d8e4be16))
* **proc:** start children in their own process group so a deadline kills the group; a process that leaves the group escapes the kill ([e56cb18](https://github.com/nution101/ttorch/commit/e56cb18540aa1bafdc495054b44ab314d9517350))


### Bug Fixes

* **approve:** fail closed when the interactive test cannot evaluate ([c808b92](https://github.com/nution101/ttorch/commit/c808b92abd5d720c32ce965ea86acae1ee4c55fc))
* **brieflint:** a run that checked nothing is not a pass ([5194be1](https://github.com/nution101/ttorch/commit/5194be1389d4c673fb168a4de2dd0a1c8b751637))
* **brieflint:** answer "which sentence" by search, and check the budget where the work is ([f571061](https://github.com/nution101/ttorch/commit/f5710616e9ac3e35cc88dfdfcf6d5e0abfabc133))
* **brieflint:** bound a prohibition per clause, and stop claiming more than that ([cc1f1c4](https://github.com/nution101/ttorch/commit/cc1f1c4f4ed2ebe4c31df648cc1f6767dbeb77fb))
* **brieflint:** bound and fail closed the git work a brief can cause ([67e3944](https://github.com/nution101/ttorch/commit/67e3944c5a403abd0922dd3a6c5e08a2a8153be5))
* **brieflint:** cap how many findings one brief can produce ([5670075](https://github.com/nution101/ttorch/commit/567007536627a5d83a09b27ebc5a0323545af8aa))
* **brieflint:** cap the config file, and report a refused one as unevaluable ([d847017](https://github.com/nution101/ttorch/commit/d847017376395e67e13240f8bb5a3a6d686e8ec9))
* **brieflint:** check the run budget inside rule 5's loop, not once before it ([73ef841](https://github.com/nution101/ttorch/commit/73ef841813aec5d54fc6475d5c3e650e17b5cbc7))
* **brieflint:** close the two lows that were mis-stating what ran ([f4971ec](https://github.com/nution101/ttorch/commit/f4971ecb8ef0c2213e0decb2579920fb9742c154))
* **brieflint:** compute sentence boundaries once, not once per item found ([a8ac3d8](https://github.com/nution101/ttorch/commit/a8ac3d86bc4e5f9bdc521656e2c1daac7948e2e1))
* **brieflint:** count and quote hard counts per sentence, not per span ([037dbba](https://github.com/nution101/ttorch/commit/037dbba4d29d6203db75fe702c1674259776fec7))
* **brieflint:** count only the rules that actually ran ([82eaa05](https://github.com/nution101/ttorch/commit/82eaa051a3329321da7a0401724416b7522052e0))
* **brieflint:** earn the create exemption per mention, attach the hedge ([979c08f](https://github.com/nution101/ttorch/commit/979c08f6868a041d44a7b8704b8c6f333bda88c3))
* **brieflint:** enforce the run budget instead of declaring it ([b43dbe1](https://github.com/nution101/ttorch/commit/b43dbe16b27aed96aa15001d0f582d583ba12a17))
* **brieflint:** guard the ls-tree ref, and assert guard position ([743a970](https://github.com/nution101/ttorch/commit/743a97040c5551c8f7ed9313f6c8896b6c663d5f))
* **brieflint:** make the text phase linear and bounded on untrusted input ([b121cdf](https://github.com/nution101/ttorch/commit/b121cdfa0fd0a4c468c673c9dac7f32cd2b33c16))
* **brieflint:** narrow every within-sentence scope to the sentence, not the span ([403fcc1](https://github.com/nution101/ttorch/commit/403fcc10d3c95098becdbc2848c904371f7f260d))
* **brieflint:** quote config values on their way to the terminal ([9e7637c](https://github.com/nution101/ttorch/commit/9e7637c84ccb4a5601c5df6951a33eca0af19782))
* **brieflint:** quote git's stderr on its way to the terminal ([7d54f9b](https://github.com/nution101/ttorch/commit/7d54f9b920ce602c17e01d11cd6b2df02eba92b6))
* **brieflint:** report what the hard-counts rule checks, not what it intends ([3f2707b](https://github.com/nution101/ttorch/commit/3f2707bdd738ba49571360bad28978f1c83de117))
* **brieflint:** resolve a line citation against the base by default ([6d0e13d](https://github.com/nution101/ttorch/commit/6d0e13d7d4a84b82c0dc09af812abb83e3e730e1))
* **brieflint:** say what a size refusal actually refused ([294d607](https://github.com/nution101/ttorch/commit/294d607fd42d849065a942437f835782b85a6ba5))
* **brieflint:** say where a bound may sit, since the rule accepts two places ([9dc5404](https://github.com/nution101/ttorch/commit/9dc5404ce0db2fc317cba714f1e00a5c9a35f28e))
* **brieflint:** say which reason kept a rule from running ([ac14cf1](https://github.com/nution101/ttorch/commit/ac14cf171c00146766c898e1aee51981c46a95b7))
* **brieflint:** split sentences the way briefs are actually written ([f7b796d](https://github.com/nution101/ttorch/commit/f7b796d382b615cd133d4b27236cb530f124816b))
* **brieflint:** stop the create exemption falling to passive voice ([cd7b498](https://github.com/nution101/ttorch/commit/cd7b4981d31b9e6a47e7e4ae0da73d18e23842ee))
* **brieflint:** stop the splitter merging two sentences into one ([c59da18](https://github.com/nution101/ttorch/commit/c59da187aee338ba775b24e4165d46ddcb5c7f54))
* **brieflint:** truncate the list of unchecked targets ([843aec7](https://github.com/nution101/ttorch/commit/843aec7530b2ac20620f10e4b2e7c3dd83f89cf9))
* **cli:** let only this package's errors choose an exit status ([cd87cac](https://github.com/nution101/ttorch/commit/cd87cac9eaf34c711174d73d28ebe655ed38d36d))
* **cli:** read every brief file under the cap, and refuse an oversize one ([d147157](https://github.com/nution101/ttorch/commit/d14715727bd2ed0c9e0bd824304dd444223ba69d))
* **content:** unexport the embedded payload behind a read accessor ([7bc7754](https://github.com/nution101/ttorch/commit/7bc775464942a30adaf39e7800463f8df7a88682))
* **gate:** --no-renames on diffLineStat too ([6866803](https://github.com/nution101/ttorch/commit/68668036e1426ecdd523e75eb32e5f16411b94a7))
* **gate:** ask git for both sides of a rename ([b03feaa](https://github.com/nution101/ttorch/commit/b03feaa9b60ceb6e79c23dbfda592ba6159e04b4))
* **gate:** block when the reports directory cannot be listed ([52cec36](https://github.com/nution101/ttorch/commit/52cec36ebe074f42ff198c57bf456501bc286e30))
* **gate:** bound the episode where every unfinished route passes ([9957ef9](https://github.com/nution101/ttorch/commit/9957ef9950363210eba493e101aec8372544ff4d))
* **gate:** carry a blocking head-pinned report across a re-prep ([8c2b564](https://github.com/nution101/ttorch/commit/8c2b5642296ab6385d605a5da679ac109038c832))
* **gate:** case-fold the gate-config match and unquote its input ([2ce0a3f](https://github.com/nution101/ttorch/commit/2ce0a3fe809bd42c99437b46dadd610b03611025))
* **gate:** charge a reviewer launch by failure kind, not uniformly ([a21345c](https://github.com/nution101/ttorch/commit/a21345c7b696b7500eafa5f87993dffa3279fc42))
* **gate:** compare paths the way APFS does, and refuse collisions outright ([c858102](https://github.com/nution101/ttorch/commit/c8581022234aae0936a1e43a295351f73138b1a2))
* **gate:** cover the evidence layer, and derive what counts as a proof ([16ab700](https://github.com/nution101/ttorch/commit/16ab7007c9cc61bfc9513c4bd7da5e635c756b60))
* **gate:** cover the gate's own proofs, and stop testing them vacuously ([c44b418](https://github.com/nution101/ttorch/commit/c44b418e8f0e0cf12e2fd1e6cf301effb823c5b8))
* **gate:** cross-check the stall clock against what a worker cannot write ([4a877dd](https://github.com/nution101/ttorch/commit/4a877ddcdb92c68356102d1c6f5f34a231c5c46a))
* **gate:** derive the required review dimensions as a floor at record time ([080371e](https://github.com/nution101/ttorch/commit/080371ef59a79a6b7d3d2487f7496d99d1d55050))
* **gate:** derive the reviewer floor over every candidate base ([083b66e](https://github.com/nution101/ttorch/commit/083b66e1d39e35678ca0e4959c6e2d09fcfa14b4))
* **gate:** drop git from the scope test so it runs in the gate lane ([b6c686e](https://github.com/nution101/ttorch/commit/b6c686edabd84240365f426fa858e61dbaa6fcae))
* **gate:** enumerate symlinks without git so the check runs in the gate lane ([5c74198](https://github.com/nution101/ttorch/commit/5c741987452218fba12789a0d0dfcb0ac29d1609))
* **gate:** fail closed when the episode's dispatch record is lost ([8875416](https://github.com/nution101/ttorch/commit/88754164878ce2aa7c4841ba89d6f23ed0986ee0))
* **gate:** fold every report pinned to head, not the ones the record lists ([83c9abf](https://github.com/nution101/ttorch/commit/83c9abf5a5e57b6f8e8b0dde7858ee671052d3e9))
* **gate:** fold Unicode, refuse colliding paths, stop forging audit records ([c8c4c92](https://github.com/nution101/ttorch/commit/c8c4c922ccce106b93384175f1b3bd122ada25e0))
* **gate:** generate the cost figures, and refuse a hand-written one anywhere ([ca2822d](https://github.com/nution101/ttorch/commit/ca2822da5ee3b4c80ee0b78098716bb2e9b1e903))
* **gate:** give the advisory audits their own review episode ([73c78c0](https://github.com/nution101/ttorch/commit/73c78c05a1f316bcf7f5691619bb610b2797f834))
* **gate:** keep a dispatched dimension required until it reports ([590c30b](https://github.com/nution101/ttorch/commit/590c30bb9feb7ea80b9720fd78ad97c325efb40c))
* **gate:** keep only the decision shape in the authority validate memo ([9f50a36](https://github.com/nution101/ttorch/commit/9f50a368200a8e4b09d81e81d13e7122702fe210))
* **gate:** keep the advisory guard up when the episode row is deleted ([76b12f3](https://github.com/nution101/ttorch/commit/76b12f37c74a23ec4fa38cbdefc2a7b4d782fc0f))
* **gate:** key collisions on directories too, and settle the fold by measurement; a blob could delete a covered directory (e.g. .github/workflows/) from the validated checkout, and that is now refused ([031af85](https://github.com/nution101/ttorch/commit/031af85f9e8c0fcf1ca29eaf21363af83de31dc9))
* **gate:** match a covered directory's own path, and refuse links over it ([c1fef23](https://github.com/nution101/ttorch/commit/c1fef23ed6a2b61ac3d852ad492b2ea9443898f8))
* **gate:** measure every published cost figure, not three table rows ([c300085](https://github.com/nution101/ttorch/commit/c300085528e54da23f2bcc4560465f1d74f797d7))
* **gate:** move skipIfShort into a covered file ([2519783](https://github.com/nution101/ttorch/commit/2519783c2fb1959fe8604a22eed554180a78682f))
* **gate:** move the episode record into the store ([2f4f01e](https://github.com/nution101/ttorch/commit/2f4f01e87c95280b9fcfcd01fb3ae36a934fff37))
* **gate:** move the reviewer's scratch workspace out of the review-inputs dir ([6b2a94f](https://github.com/nution101/ttorch/commit/6b2a94fcbb219569b2040827ef71801d4ea0f68b))
* **gate:** never discard a report from a reviewer the gate dispatched ([6cf84b9](https://github.com/nution101/ttorch/commit/6cf84b9daac8664c7d5fc9d0470bd339c5565a69))
* **gate:** reach the covered-path lists, and sweep the proofs the symbols miss ([d4793c0](https://github.com/nution101/ttorch/commit/d4793c04e2b1f77894098f8b27d4dbfb799c3d35))
* **gate:** read the scope signal from the raw blob, not from git grep ([74da5d4](https://github.com/nution101/ttorch/commit/74da5d46226368bcb7adde4d5c1edc29fcd33e46))
* **gate:** refuse a grant path that the token parser would split ([56c23d1](https://github.com/nution101/ttorch/commit/56c23d11e3da4295f113fa7d5422934bb5ae3281))
* **gate:** refuse a review workspace whose ancestors carry session config ([ec60f14](https://github.com/nution101/ttorch/commit/ec60f1415f653bdd789553a265a99bc72abcb001))
* **gate:** refuse a review workspace whose mirror lacks the reviewed commit ([700f914](https://github.com/nution101/ttorch/commit/700f914cd8248ee4c5ae590e2359fe6041e9b3c9))
* **gate:** require the green that authorizes a merge to be a run this process made ([076a44f](https://github.com/nution101/ttorch/commit/076a44fa3533493277eb226477616a5f372ff61b))
* **gate:** return the error when an episode write fails ([750da3f](https://github.com/nution101/ttorch/commit/750da3f4ae999c14fb87eb7f8bdd279dfb5f9a46))
* **gate:** run every reviewer outside the worker's worktree ([5862769](https://github.com/nution101/ttorch/commit/5862769b9ff998dbad9bae9f925a278aa9b02f4b))
* **gate:** run the security reviewer outside the worker's worktree ([e358bf4](https://github.com/nution101/ttorch/commit/e358bf4045ed3fb13d30aa858db74df2bf23d413))
* **gate:** run the stall clock from the episode, not the first dispatch ([e6e5b4a](https://github.com/nution101/ttorch/commit/e6e5b4a222ecbfaf32333c594c1f416a2c6e944e))
* **gate:** scope the covered set to the repository it is gating ([3993158](https://github.com/nution101/ttorch/commit/39931580dda3d663404f47dea4985b9dc91dc33f))
* **gate:** stop the advisory guard resting on prep.json, which one rm could delete; it reads the store episode first ([9482cbf](https://github.com/nution101/ttorch/commit/9482cbfe75e8df3a723bf70c0c9c3574ed7bacd5))
* **gate:** stop the advisory prep resetting a live gate episode ([0a30cd1](https://github.com/nution101/ttorch/commit/0a30cd13ecbe79ba7effa8759782a2e8d21a84e8))
* **gate:** stop the land pass staging a reused green over prep's validate ([d741aaf](https://github.com/nution101/ttorch/commit/d741aafe292f60c79d581e25e7b6ccde303988c9))
* **gate:** stop the reviewer brief presenting validate.json as proof ([d7ba85d](https://github.com/nution101/ttorch/commit/d7ba85dff13637b974975e961837516cd6b76a04))
* **gate:** validate the task id before it becomes a delete path ([6548ba1](https://github.com/nution101/ttorch/commit/6548ba184c1fd787a5fe67ff5ac33aed438cf18a))
* **gate:** validate the workspace dimension with the shared predicate ([d4c3b34](https://github.com/nution101/ttorch/commit/d4c3b345ab42d408a859232756e0574219b8dea5))
* **gate:** walk the reviewer workspace's resolved path, not its logical one ([01ffc1d](https://github.com/nution101/ttorch/commit/01ffc1dfccdab2ce64758e04e63114c9cc2bbb9d))
* **gate:** write the reviewer's prompt 0600 into a 0700 workspace ([466f4eb](https://github.com/nution101/ttorch/commit/466f4ebc669d7eabebce64a446ab28fd55d41d89))
* **gate:** write the reviewer's prompt into its own per-dispatch workspace, not the review-inputs dir ([35568d1](https://github.com/nution101/ttorch/commit/35568d1706f119beb0d32838e13967d5ab2cab17))
* **installer:** choose the embedded payload inside the covered package ([12219b0](https://github.com/nution101/ttorch/commit/12219b0a138fda77ee0c78841544a46b08ba5047))
* **proc:** bind the group kill to the command it was installed on ([c15e1b9](https://github.com/nution101/ttorch/commit/c15e1b9af9d131c86a2a515a6f5074185318cb9e))
* **proc:** read the Cancel binding instead of firing it, and stop discarding findings ([fae0556](https://github.com/nution101/ttorch/commit/fae0556d4cf7640a8fa7c39c0a01b6d77a659d0a))
* **review:** archive the dimensions the previous prep prepared ([bc06caf](https://github.com/nution101/ttorch/commit/bc06caf03a0d7017b16bd0af9745d6b68cf1aa8a))
* **review:** block a verdict over an empty dimension set ([46acd95](https://github.com/nution101/ttorch/commit/46acd95d77dc67bf263bf3ddd693cd9472e7ea26))
* **review:** block when an extra dimension's report cannot be read ([7eb8ee7](https://github.com/nution101/ttorch/commit/7eb8ee7c48f9113cc8b24f764eb4f515013e44fd))
* **review:** corroborate the prep marker against the staged validate ([bb9aaeb](https://github.com/nution101/ttorch/commit/bb9aaebe2b006cf93aae3d12a00cbba4d19b8874))
* **review:** cover every subtree the installer writes into a session ([6baa1d9](https://github.com/nution101/ttorch/commit/6baa1d9b60eb91f0e7c21b1ac79e9e4f48607fcd))
* **review:** fold the directory segment, not just the basename ([6841b36](https://github.com/nution101/ttorch/commit/6841b363751c25707c22e737c82e6152ee3010b4))
* **review:** guard the manual path and widen the sink scan ([b211df3](https://github.com/nution101/ttorch/commit/b211df3f58838be3b59f7cf3f846ecd02ab82bfe))
* **review:** keep reports in their own directory, away from the control files ([a0621d5](https://github.com/nution101/ttorch/commit/a0621d5e1f48d655f672ad89e868b70065ce8c8e))
* **review:** quote severity too, and strip the separators SafeLine missed ([f9afe2e](https://github.com/nution101/ttorch/commit/f9afe2ee94300584701f726f6a037efb7601e4d7))
* **review:** quote the filename the legacy sweep reports on ([cc08055](https://github.com/nution101/ttorch/commit/cc08055d87a3952a9f46060a3f00308cbd9c73f4))
* **review:** refuse a clean verdict when the staged validate is not green ([4a8cfec](https://github.com/nution101/ttorch/commit/4a8cfec5cd1678d5fce32b16b8f205ad57198b03))
* **review:** render reviewer free text as quoted data, not as output ([2aa4748](https://github.com/nution101/ttorch/commit/2aa4748307dab6189626c94f9a3ede09133ad282))
* **review:** spare the advisory verdict files by name, not by luck ([8113c37](https://github.com/nution101/ttorch/commit/8113c37010d40aeb46f0cf1f40369edbe765081e))
* **review:** stop agent instruction files classifying as inert prose ([cf88074](https://github.com/nution101/ttorch/commit/cf880749356fd2e4deb0ee508242fadd74d10b5b))
* **review:** take the required reviewer set from the prep stamp, not the file ([22746ee](https://github.com/nution101/ttorch/commit/22746eec3d1d738509703e0b76ee2deb93208ed1))
* **review:** treat a report from a superseded prep as no review at all ([8fcc5be](https://github.com/nution101/ttorch/commit/8fcc5be36dfc9bf19a991ca842a52bbc327344cf))
* **review:** treat agent configuration as code, not inert prose ([6e19dda](https://github.com/nution101/ttorch/commit/6e19ddae72aa2df6a8275d259d5b73d4b1e6fb6e))
* **review:** validate dimension names at every sink, not just the archive ([aef35c0](https://github.com/nution101/ttorch/commit/aef35c0853771d220ef296d9e472f09987d72aa6))
* **review:** validate dimension names before they become paths ([9077f82](https://github.com/nution101/ttorch/commit/9077f8229831cd60379471f0716836b7d180f3bc))
* **review:** validate the dimension set before composing the block payload ([9c32075](https://github.com/nution101/ttorch/commit/9c320755f75f06fb189e9d2de5680fe4e8831f8e))
* **scheduler:** bound how long one gate pass can hold the tick ([587ff86](https://github.com/nution101/ttorch/commit/587ff863d4e2cc7c6c62d5333dae050b4021576a))
* **termtab:** detect iTerm2 installed in ~/Applications ([91847a2](https://github.com/nution101/ttorch/commit/91847a2bc85c6018649a9bbefda348857644f54b))
* **termtab:** disclose the switch-client escape, and stop a wedged probe eating a retry ([e4b7f0a](https://github.com/nution101/ttorch/commit/e4b7f0a4658449cca73eaa0c0953169015be900c))
* **termtab:** make the worker view tab read-only on tmux 3.2 or later, so a stray keystroke no longer reaches the worker ([48e0f30](https://github.com/nution101/ttorch/commit/48e0f3093f873a2b27c6e5503d2c71d24d989789))
* **tmux:** bound every tmux command, and stop the view gate failing open ([9cff456](https://github.com/nution101/ttorch/commit/9cff456bb0ab669d25f076a82229b3897f290be5))
* **tmux:** make a timeout distinguishable, and bound the last tmux call ([0390e5e](https://github.com/nution101/ttorch/commit/0390e5ecb969d75955b62d0a340f1ef026d5739c))
* **validate:** say what the pre-start check actually verifies ([74b315a](https://github.com/nution101/ttorch/commit/74b315ae105680d48b6bb944039883eb48aa3828))
* **watch:** make a refused singleton acquisition exit non-zero and say so ([36c3a96](https://github.com/nution101/ttorch/commit/36c3a963342cda1756a081fc305508b30e9f19bf))
* **worktree:** frame the batch blob read so it cannot desync or fail open ([0c25adc](https://github.com/nution101/ttorch/commit/0c25adc60f4834db991a9a2f36be54c83342d831))
* **worktree:** read base blobs by object id, not by path ([3aa2ca1](https://github.com/nution101/ttorch/commit/3aa2ca1017d44507ddab1bb695a13d9908f94a0c))
* **worktree:** reject a malformed size, and check ls-tree like its sibling ([fbebbd6](https://github.com/nution101/ttorch/commit/fbebbd60086bdffd9078d854c4956afa150e02fc))

## [0.17.0](https://github.com/nution101/ttorch/compare/v0.16.2...v0.17.0) (2026-07-15)


### Features

* autonomous trusted-gating + tree-hash validate cache ([437a0d5](https://github.com/nution101/ttorch/commit/437a0d564dacd5ff17f75c66958dcb7b9acc9f24))
* **gate:** content-address the validate cache by git tree hash ([ba1b124](https://github.com/nution101/ttorch/commit/ba1b1242ad46039dcaad44482ceba7761b7121ac))
* **scheduler:** auto-gate trusted done-work in the autostarted daemon ([bf44830](https://github.com/nution101/ttorch/commit/bf448305ec4ec9d0ab282496442ee9d9272a1686))

## [0.16.2](https://github.com/nution101/ttorch/compare/v0.16.1...v0.16.2) (2026-07-08)


### Bug Fixes

* **worker:** enforce ttorch report via a worker Stop hook ([682beb8](https://github.com/nution101/ttorch/commit/682beb8f852baefccbdc343aedead405739d6731))
* **worker:** enforce ttorch report via a worker Stop hook ([9e090ee](https://github.com/nution101/ttorch/commit/9e090eea5a570417605e31155a0aae8326fadda3))

## [0.16.1](https://github.com/nution101/ttorch/compare/v0.16.0...v0.16.1) (2026-07-05)


### Bug Fixes

* **model:** never write code below opus; confine sonnet to research ([371b764](https://github.com/nution101/ttorch/commit/371b76403d8172d044f808d2eaf090f30a3c0ebc))
* **model:** never write code below opus; confine sonnet to research ([087b417](https://github.com/nution101/ttorch/commit/087b417685c288956484f24e5562ab0cc816ffdd))

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
