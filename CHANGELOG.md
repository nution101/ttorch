# Changelog

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
