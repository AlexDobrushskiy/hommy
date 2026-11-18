# Workslots

Parallel lanes need checkouts of their own (master §14 D-22). A workslot is a git worktree of `origin/main` with its
own branch, ports and scratch root; slot 0 is the phase checkout itself.

| Slot | Checkout | Branch | Ports | Scratch root |
|---|---|---|---|---|
| 0 | the main checkout (`app/`) | the phase's | 4710–4719 | `/tmp/hm-s0/` |
| N (1–9) | `<main checkout>-slots/slot<N>` | `slot<N>/<slug>` | `4710+10N` … `+9` | `/tmp/hm-s<N>/` |

```sh
scripts/slot.sh claim my-task [--slot N]   # prints the new checkout's dir (N: the lowest free slot)
scripts/slot.sh status [N]                 # phase | free | taken, branch, ports, scratch root
scripts/slot.sh list
scripts/slot.sh path N
scripts/slot.sh release N [--force]        # test env down --purge, worktree and branch removed: `free`
```

- Inside a slot's checkout every harness script picks its slot from the directory name (`HOMMY_SLOT` overrides), so
  `scripts/test-env.sh up`, `scripts/tg.sh`, `scripts/sx.sh` need no `--slot`.
- `release` refuses a dirty tree or commits that are neither in `origin/main` nor pushed; `--force` discards them.
- Slot 0 is never claimed or released. Pushes from a slot go through the same hooks (shared git config).
- Assignments: 03 uses slot 1's ports from its own repo; 04 runs in slot 2; 07's prep in slot 3 (testing.md §2).
