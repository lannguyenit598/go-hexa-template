---
paths:
  - "**/*.md"
  - "api.http"
---

# Documentation

- Keep both README versions aligned: `README.md` is English and the GitHub default; `README.vi.md` is the sole Vietnamese exception. Update both when behavior changes; an English-only translation does not require rewriting the Vietnamese version.
- All code comments, documentation, examples, configuration guidance, and agent instructions must be in English, except `README.vi.md`. Give each document a clear audience; link instead of duplicating content.
- Verify behavioral claims (status codes, literal `detail`, headers, defaults, commands, SQL) by running them before documenting new behavior. Run user-facing scripts and SQL exactly as written.
- After editing, verify all relative links and `#...` anchors.
