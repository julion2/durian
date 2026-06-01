---
title: Durian
toc: false
width: wide
---

<style>
/* Hero cycler — single line, cycles through 6 keymaps via CSS keyframes */
.durian-cycler {
  position: relative;
  height: 4rem;
  font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, monospace;
}
.durian-cycler-item {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  gap: 1rem;
  opacity: 0;
  animation: durian-cycle 15s infinite;
}
.durian-cycler-key {
  display: inline-block;
  padding: 0.35rem 0.85rem;
  border-radius: 0.5rem;
  border: 1px solid rgb(229, 231, 235);
  background: rgba(255, 255, 255, 0.5);
  font-weight: 600;
  font-size: 1.5rem;
  line-height: 1;
  min-width: 3.25rem;
  text-align: center;
}
.dark .durian-cycler-key {
  border-color: rgb(64, 64, 64);
  background: rgba(23, 23, 23, 0.5);
}
.durian-cycler-arrow { color: rgb(156, 163, 175); font-size: 1.25rem; }
.durian-cycler-label { font-size: 1.5rem; font-weight: 500; letter-spacing: -0.01em; }
.dark .durian-cycler-label { color: rgb(229, 229, 229); }

@keyframes durian-cycle {
  0%, 1%       { opacity: 0; transform: translateY(0.5rem); }
  3%, 14%      { opacity: 1; transform: translateY(0); }
  16%, 100%    { opacity: 0; transform: translateY(-0.5rem); }
}
.durian-cycler-item:nth-child(1) { animation-delay: 0s; }
.durian-cycler-item:nth-child(2) { animation-delay: 2.5s; }
.durian-cycler-item:nth-child(3) { animation-delay: 5s; }
.durian-cycler-item:nth-child(4) { animation-delay: 7.5s; }
.durian-cycler-item:nth-child(5) { animation-delay: 10s; }
.durian-cycler-item:nth-child(6) { animation-delay: 12.5s; }

/* Pkl card */
.durian-pkl {
  border-radius: 0.875rem;
  border: 1px solid rgb(229, 231, 235);
  background: rgb(250, 250, 250);
  overflow: hidden;
  box-shadow: 0 1px 3px rgba(0,0,0,0.04);
}
.dark .durian-pkl { border-color: rgb(38, 38, 38); background: rgb(17, 17, 17); }
.durian-pkl-chrome {
  display: flex; align-items: center; gap: 0.5rem;
  padding: 0.65rem 0.9rem;
  border-bottom: 1px solid rgb(229, 231, 235);
  font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, monospace;
  font-size: 0.75rem;
  color: rgb(115, 115, 115);
}
.dark .durian-pkl-chrome { border-bottom-color: rgb(38, 38, 38); }
.durian-pkl-dot { width: 0.65rem; height: 0.65rem; border-radius: 50%; background: rgb(228, 228, 231); }
.dark .durian-pkl-dot { background: rgb(64, 64, 64); }
.durian-pkl-path { margin-left: 0.5rem; }
.durian-pkl pre {
  margin: 0 !important;
  padding: 1rem 1.1rem !important;
  background: transparent !important;
  font-size: 0.85rem;
  line-height: 1.55;
}

/* Hero — capped height with content-driven grid that ACTUALLY goes 2-column
   on desktop. Hextra's pre-compiled CSS has no `lg:` breakpoint variants,
   so the grid layout has to be hand-rolled. */
.durian-hero {
  position: relative;
  display: flex;
  flex-direction: column;
  justify-content: center;
  min-height: calc(100vh - var(--navbar-height, 60px));
  padding: 2rem 0 1.5rem;
}

.durian-hero-subtitle {
  max-width: 38rem;
  margin: 0 auto 1.5rem;
}

.durian-hero-stack {
  display: flex;
  flex-direction: column;
  align-items: stretch;
  gap: 0.75rem;
  max-width: 40rem;
  margin: 0 auto 1.5rem;
  width: 100%;
}

/* Scroll indicator — absolute, fades on scroll past hero */
.durian-scroll {
  position: absolute;
  bottom: 1.25rem;
  left: 50%;
  transform: translateX(-50%);
  color: rgb(156, 163, 175);
  animation: durian-bounce 2.4s ease-in-out infinite;
  transition: opacity 0.4s ease, visibility 0.4s ease;
  pointer-events: none;
}
.durian-scroll.is-hidden { opacity: 0; visibility: hidden; }
@keyframes durian-bounce {
  0%, 100% { transform: translateX(-50%) translateY(0); opacity: 0.45; }
  50%      { transform: translateX(-50%) translateY(0.4rem); opacity: 0.9; }
}

/* Hero button row */
.durian-btn-row {
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  gap: 1.25rem;
  margin-bottom: 3rem;
}

/* Hero buttons */
.durian-btn {
  display: inline-flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.7rem 1.4rem;
  border-radius: 9999px;
  font-weight: 500;
  font-size: 0.95rem;
  transition: all 0.2s ease;
  cursor: pointer;
}
.durian-btn-primary {
  background: linear-gradient(135deg, #A78BFA 0%, #8B5CF6 100%);
  color: rgb(250, 250, 250);
  border: 1px solid #8B5CF6;
  box-shadow: 0 1px 2px rgba(139, 92, 246, 0.2), 0 6px 18px -3px rgba(203, 188, 232, 0.55);
}
.durian-btn-primary:hover {
  background: linear-gradient(135deg, #8B5CF6 0%, #7C3AED 100%);
  transform: translateY(-1px);
  box-shadow: 0 2px 4px rgba(124, 58, 237, 0.25), 0 10px 24px -3px rgba(203, 188, 232, 0.7);
}
.dark .durian-btn-primary {
  background: linear-gradient(135deg, #C4B5FD 0%, #A78BFA 100%);
  border-color: #A78BFA;
  color: rgb(30, 27, 75);
  box-shadow: 0 1px 2px rgba(167, 139, 250, 0.3), 0 6px 18px -3px rgba(167, 139, 250, 0.45);
}
.dark .durian-btn-primary:hover {
  background: linear-gradient(135deg, #DDD6FE 0%, #C4B5FD 100%);
}
.durian-btn-secondary {
  background: transparent;
  color: rgb(64, 64, 64);
  border: 1px solid rgb(212, 212, 216);
}
.durian-btn-secondary:hover {
  border-color: rgb(115, 115, 115);
  background: rgba(0, 0, 0, 0.02);
  transform: translateY(-1px);
}
.dark .durian-btn-secondary {
  color: rgb(212, 212, 216);
  border-color: rgb(64, 64, 64);
}
.dark .durian-btn-secondary:hover {
  border-color: rgb(115, 115, 115);
  background: rgba(255, 255, 255, 0.04);
}
</style>

<div class="durian-hero">

<div class="hx:text-center hx:mb-4">
<img src="logo.png" alt="Durian" width="76" height="76" class="hx:mx-auto hx:rounded-2xl hx:shadow-sm" />
</div>

<div class="hx:mb-4 hx:text-center">
{{< hextra/hero-badge >}}Early Alpha · macOS{{< /hextra/hero-badge >}}
</div>

<div class="hx:mb-3 hx:text-center">

{{< hextra/hero-headline >}}Mail you can&nbsp;grep.{{< /hextra/hero-headline >}}

</div>

<div class="hx:text-center durian-hero-subtitle">

{{< hextra/hero-subtitle >}}Use it from the terminal, in a native macOS app, or&nbsp;both. Vim&nbsp;keys, tags instead of folders, local SQLite, typed Pkl&nbsp;config.{{< /hextra/hero-subtitle >}}

</div>

<div class="durian-hero-stack">

<div class="durian-cycler">
  <div class="durian-cycler-item">
    <span class="durian-cycler-key">gi</span>
    <span class="durian-cycler-arrow">→</span>
    <span class="durian-cycler-label">go to inbox</span>
  </div>
  <div class="durian-cycler-item">
    <span class="durian-cycler-key">dd</span>
    <span class="durian-cycler-arrow">→</span>
    <span class="durian-cycler-label">archive thread</span>
  </div>
  <div class="durian-cycler-item">
    <span class="durian-cycler-key">c</span>
    <span class="durian-cycler-arrow">→</span>
    <span class="durian-cycler-label">compose</span>
  </div>
  <div class="durian-cycler-item">
    <span class="durian-cycler-key">r</span>
    <span class="durian-cycler-arrow">→</span>
    <span class="durian-cycler-label">reply</span>
  </div>
  <div class="durian-cycler-item">
    <span class="durian-cycler-key">t</span>
    <span class="durian-cycler-arrow">→</span>
    <span class="durian-cycler-label">tag picker</span>
  </div>
  <div class="durian-cycler-item">
    <span class="durian-cycler-key">/</span>
    <span class="durian-cycler-arrow">→</span>
    <span class="durian-cycler-label">search</span>
  </div>
</div>

<div class="durian-pkl">
  <div class="durian-pkl-chrome">
    <span class="durian-pkl-dot"></span>
    <span class="durian-pkl-dot"></span>
    <span class="durian-pkl-dot"></span>
    <span class="durian-pkl-path">~/.config/durian/config.pkl</span>
  </div>

```pkl
accounts {
  (C.gmail) {
    name = "Personal"
    alias = "personal"
    email = "you@gmail.com"
  }
  (C.microsoft365) {
    name = "Work"
    alias = "work"
    default = true
    email = "you@company.com"
  }
}
```

</div>

</div>

<div class="durian-btn-row not-prose" style="margin-bottom: 2rem;">
  <a href="docs/getting-started/" class="durian-btn durian-btn-primary">
    Get Started
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12"></line><polyline points="12 5 19 12 12 19"></polyline></svg>
  </a>
  <a href="https://github.com/julion2/durian" target="_blank" rel="noreferrer" class="durian-btn durian-btn-secondary">
    <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor"><path d="M12 .3a12 12 0 0 0-3.8 23.4c.6.1.8-.3.8-.6v-2c-3.3.7-4-1.6-4-1.6-.6-1.4-1.4-1.8-1.4-1.8-1.1-.7.1-.7.1-.7 1.2.1 1.9 1.2 1.9 1.2 1.1 1.9 2.9 1.4 3.6 1 .1-.8.4-1.4.8-1.7-2.7-.3-5.5-1.3-5.5-6 0-1.3.5-2.4 1.2-3.2-.1-.3-.5-1.5.1-3.2 0 0 1-.3 3.3 1.2a11.5 11.5 0 0 1 6 0c2.3-1.5 3.3-1.2 3.3-1.2.7 1.7.2 2.9.1 3.2.8.8 1.2 1.9 1.2 3.2 0 4.6-2.8 5.6-5.5 5.9.4.4.8 1.1.8 2.2v3.3c0 .3.2.7.8.6A12 12 0 0 0 12 .3"></path></svg>
    GitHub
  </a>
</div>

<div id="durian-scroll-indicator" class="durian-scroll">
  <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.25" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"></polyline></svg>
</div>

</div>

<script>
(function() {
  var chevron = document.getElementById('durian-scroll-indicator');
  if (!chevron) return;
  var hero = chevron.closest('.durian-hero');
  var next = hero ? hero.nextElementSibling : null;
  while (next && (next.tagName === 'BR' || !next.offsetHeight)) next = next.nextElementSibling;
  if (next && 'IntersectionObserver' in window) {
    var io = new IntersectionObserver(function(entries) {
      entries.forEach(function(e) { chevron.classList.toggle('is-hidden', e.isIntersecting); });
    }, { threshold: 0.05 });
    io.observe(next);
  } else {
    window.addEventListener('scroll', function() {
      chevron.classList.toggle('is-hidden', window.scrollY > 120);
    }, { passive: true });
  }
})();
</script>

![Durian, light mode](images/screenshot-light.png)

<details class="hx:mt-6">
<summary>More screenshots</summary>

<br>

![Durian, dark mode](images/screenshot-dark.png)

<br>

![Durian compose editor](images/screenshot-compose.png)

</details>

<br>

{{< hextra/hero-section heading="h2" >}}Mail clients keep growing into productivity suites.{{< /hextra/hero-section >}}

Calendars. Tasks. AI replies. "Engagement metrics." Sender‑side read receipts. Upsells.

Durian goes the other way: render mail, search mail, reply, file. Tags instead of folders. A SQLite store on disk. A vim‑style keyboard. Configuration in a typed file you check into git, not a settings panel you click through twice a year.

<br>

{{< hextra/feature-grid cols="2" >}}

{{< hextra/feature-card
    link="docs/developers/architecture"
    title="Two parts, one tool"
    subtitle="The `durian` CLI does the work — IMAP sync, SMTP send, SQLite store, full‑text search, HTTP API. The macOS GUI is a thin SwiftUI layer that talks to the CLI over localhost. Use either alone, or both together. They share the same Pkl config." >}}

{{< hextra/feature-card
    link="docs/cli"
    title="Local‑first, always offline"
    subtitle="Mail is fetched once and stored in SQLite. Search runs against the local FTS5 index — no round‑trip to a server. The IDLE watcher keeps the inbox fresh in the background; everything else works on a plane." >}}

{{< hextra/feature-card
    link="docs/gui/search"
    title="Tags, not folders"
    subtitle="Folders force a one‑to‑one filing system. Tags don't. A message can be `tag:invoice tag:vendor/acme tag:2026` at the same time, and your sidebar can be any saved query — `group:vip AND tag:unread`, `subject:contract has:attachment:pdf`, whatever fits how you actually work." >}}

{{< hextra/feature-card
    link="docs/configuration"
    title="Configured like infrastructure"
    subtitle="Accounts, sync intervals, filter rules, key bindings, sidebar profiles — everything is in typed Pkl files. Validated at startup, versionable, reproducible across machines. There is no settings UI on purpose." >}}

{{< hextra/feature-card
    link="docs/cli/encryption-at-rest"
    title="Encrypted at rest, searchable anyway"
    subtitle="Mail bodies, subjects, headers, contacts, drafts — all encrypted in the SQLite store with per-column sub-keys derived from a master in the OS keychain. Full-text search still works against a blind-token index, no plaintext leaves the encryption layer." >}}

{{< /hextra/feature-grid >}}

<br>

{{< hextra/hero-section heading="h2" >}}Search like notmuch.{{< /hextra/hero-section >}}

```text
group:vip AND tag:unread
from:boss@company.com AND has:attachment:pdf
date:6m.. AND subject:invoice
header:list-id: AND NOT tag:newsletter
```

Same syntax in the search popup, in `durian search`, and as folder definitions in `profiles.pkl`. Save any query as a sidebar entry — your inbox shape is just a file.

<br>

{{< hextra/hero-section heading="h2" >}}What's not in here.{{< /hextra/hero-section >}}

- No AI replies, AI summaries, AI anything.
- No remote image loading by default — no tracker pixels phoning home.
- No engagement metrics. No read receipts shipped to senders.
- No calendar, tasks, chat, or "smart" inbox. It's an email client.
- No telemetry. The app never talks to a Durian server, because there isn't one.

<br>

{{< hextra/hero-section heading="h2" >}}Install{{< /hextra/hero-section >}}

{{< callout type="warning" >}}
**Early Alpha.** Expect bugs and breaking changes. No external security audit. This is a side project — features and fixes happen as time allows.
{{< /callout >}}

```bash
brew install pkl                # config language runtime (required)
brew tap julion2/tap
brew install durian             # CLI (required — the GUI uses it as backend)
brew install --cask durian      # GUI (macOS only)
```

Or [build from source](docs/getting-started/#1-install-the-cli).

<br>

{{< hextra/hero-section heading="h2" >}}Read on.{{< /hextra/hero-section >}}

{{< cards >}}
  {{< card link="docs/getting-started" title="Getting Started" icon="lightning-bolt"
      subtitle="Install, configure your first account, send mail." >}}
  {{< card link="docs/gui" title="GUI" icon="desktop-computer"
      subtitle="Compose, sidebar, search, drafts, attachments, keymaps." >}}
  {{< card link="docs/cli" title="CLI" icon="terminal"
      subtitle="Every durian subcommand with practical examples." >}}
  {{< card link="docs/configuration" title="Configuration" icon="cog"
      subtitle="Typed Pkl files for accounts, rules, profiles, groups." >}}
  {{< card link="docs/auth" title="Authentication" icon="lock-closed"
      subtitle="OAuth (Gmail, Microsoft 365) or password." >}}
  {{< card link="docs/developers/architecture" title="Architecture" icon="cube-transparent"
      subtitle="How the CLI, GUI, and IMAP sync fit together." >}}
{{< /cards >}}
