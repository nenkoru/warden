---
name: mac-hygiene
description: What on a Mac is safe to clean (regenerable caches via their own tools) and what is the owner's (user data, apps' caches, trash).
---
# Mac hygiene

- Safe, via the tool's own command only: Go build/module caches, npm/pnpm/yarn/uv/pip caches, unavailable simulators.
- The owner's: ~/Library/Caches of apps, Hugging Face models, Xcode DerivedData while a build is open, the Trash, anything in ~/Documents, ~/Downloads, ~/Desktop.
- Homebrew may run as another user: never run brew; report `brew cleanup` for the owner.
- Low disk: report the biggest regenerable caches first, then the owner's candidates with sizes.
