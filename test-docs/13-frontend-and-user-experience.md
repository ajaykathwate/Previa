# 13 · Frontend and user experience

**Module:** UI · **Code:** `web/` (templates, `static/js/ui.js`, CSS), handlers'
view models · **Feature docs:** [21 · UI architecture](../docs/21-ui-architecture.md),
[User guide](../technical-docs/user-guide.md)

What a person sees and can do: navigation, modals, keyboard and screen-reader
use, small screens, no JavaScript, and above all what happens when the network or
the server misbehaves (slow, refused, failed, offline or unanswered requests,
double clicks). Every browser case runs in Chromium under the production
Content-Security-Policy, and any JavaScript error or CSP violation fails it.

| Sub-module | Prefix | Cases |
| --- | --- | --- |
| Navigation and layout | TC-UI-NAV | 5 |
| Modals and confirmations | TC-UI-MOD | 2 |
| Request resilience | TC-UI-RES | 8 |
| Accessibility | TC-UI-A11Y | 3 |
| Without JavaScript | TC-UI-NOJS | 3 |
| Honest states | TC-UI-STATE | 6 |
| Untrusted data | TC-UI-SAFE | 1 |

## 1. Navigation and layout (TC-UI-NAV)

- [x] **TC-UI-NAV-001 · Public pages are open; the workspace needs sign-in**  
  Type: Positive · Priority: P1 · Level: Browser  
  Steps: Visit the home page, its anchors and `/faq` signed out; request the dashboard, repositories, Builds, Previews and developer tools.  
  Expected: The home page and FAQ are public; every workspace page redirects to sign-in; the public home offers no workspace actions.  
  Evidence: browser › *public home and protected dashboard access*; `internal/dashboard` › `TestHomeUsesLayout`; `internal/dashboard` › `TestFAQIsPublicOnHomeAndAPageInTheWorkspace`

- [x] **TC-UI-NAV-002 · Layouts work from 320 px phones to desktops**  
  Type: Usability · Priority: P1 · Level: Browser  
  Steps: Open the dashboard, workspace and PR pages at 320, 390, 768 and 1440 px and on short screens.  
  Expected: No horizontal overflow; sticky navigation stays at the top; filters align on desktop; tables stay usable.  
  Evidence: browser › *responsive dashboard sticky navigation and honest placeholders*; browser › *full pages align desktop filters and remain usable on short mobile and tablet screens*; browser › *PR workspace live search pagination modal and build control*

- [x] **TC-UI-NAV-003 · Mobile navigation and the command palette**  
  Type: Usability · Priority: P2 · Level: Browser  
  Steps: Open sections from the mobile menu; open the command palette with the keyboard, filter, navigate and close it.  
  Expected: Sections open as separate pages and the destination is highlighted; the palette filters, navigates and restores focus.  
  Evidence: browser › *mobile navigation opens separate pages and highlights the destination*; browser › *command palette filters destinations, navigates and restores focus*; `web/layouts` › `TestNavigationSelectionDoesNotDependOnTitle`

- [x] **TC-UI-NAV-004 · Breadcrumbs, history and bookmarks keep the person's place**  
  Type: Usability · Priority: P2 · Level: Browser  
  Steps: Filter, open a PR, go back through breadcrumbs and browser history; open old query-style bookmarks.  
  Expected: Filters survive navigation and history; old bookmarks redirect to the dedicated pages.  
  Evidence: browser › *dedicated repository and PR pages preserve filters through breadcrumbs and history*; browser › *failed PR searches preserve records and query bookmarks redirect to dedicated pages*; browser › *sync and automatic controls appear only on the PR list and breadcrumbs use consistent sizing*

- [x] **TC-UI-NAV-005 · The colour theme switches and persists**  
  Type: Usability · Priority: P3 · Level: Browser  
  Steps: Toggle the theme and reload.  
  Expected: The theme changes and is remembered.  
  Evidence: browser › *theme toggle switches and persists the colour theme*

## 2. Modals and confirmations (TC-UI-MOD)

- [x] **TC-UI-MOD-001 · Destructive actions confirm in an accessible modal over the page**  
  Type: Usability / Safety · Priority: P1 · Level: Browser  
  Steps: Start a teardown, an outbound removal, a secret deletion, enabling Automatic and sign-out; cancel each with Escape or Cancel; confirm.  
  Expected: Each opens as a modal over the current page (no navigation) and changes nothing when cancelled; focus is trapped (sign-out, Automatic) and returned to the trigger (sign-out, teardown); sign-out keeps the scroll position.  
  Evidence: browser › *owner builds once and tears a preview down only after confirming in a modal*; browser › *owners approve outbound destinations and remove them only after confirming in a modal*; browser › *repository secrets editor saves several at once, never shows values, and deletes selected ones*; browser › *logout modal preserves page scroll and traps and restores focus*; browser › *Automatic confirms before enabling saves in place and live updates respect modal focus*

- [x] **TC-UI-MOD-002 · Live updates never steal focus from an open modal**  
  Type: Usability · Priority: P2 · Level: Browser  
  Steps: Keep the Automatic modal open while a live update arrives.  
  Expected: The update waits until the modal closes; focus stays in the modal.  
  Evidence: browser › *Automatic confirms before enabling saves in place and live updates respect modal focus*

## 3. Request resilience (TC-UI-RES)

- [x] **TC-UI-RES-001 · Slow searches show progress and only the newest results win**  
  Type: Resilience / Usability · Priority: P1 · Level: Browser (throttled requests)  
  Steps: Delay search responses by 900 ms; type one query, then another before the first answers; wait for the late answer.  
  Expected: The results are marked busy and say "Updating…"; the field stays editable; the newer query replaces the older request; the late answer never overwrites the newest results; the URL holds the newest query.  
  Evidence: browser › *slow searches show progress, apply only the newest results and stay responsive*

- [x] **TC-UI-RES-002 · A double click on Build sends one request**  
  Type: Negative · Priority: P1 · Level: Browser (throttled requests)  
  Steps: Delay the build response by 800 ms; double-click Build.  
  Expected: The button is disabled while the request runs; exactly one request reaches the server; the PR shows Queued.  
  Evidence: browser › *a slow build request disables its control so a double click sends one request*

- [x] **TC-UI-RES-003 · Refused, failed and offline actions explain themselves**  
  Type: Negative / Resilience · Priority: P1 · Level: Browser (intercepted requests)  
  Steps: Make Build answer 409 with a sentence, then 500 with an HTML error page, then drop the connection, then work normally.  
  Expected: The 409 sentence is shown; the 500 and the dropped connection show "Could not update this view. Check your connection…" and never the error page; each time the panel is not left busy, the button works again and nothing changed; once back online the build is queued and the message clears.  
  Evidence: browser › *refused, failed and offline requests explain themselves and leave the page usable*

- [x] **TC-UI-RES-004 · A request that never answers times out and can be retried**  
  Type: Resilience · Priority: P2 · Level: Browser (intercepted requests)  
  Steps: Leave one search unanswered; wait past the 10-second limit; search again.  
  Expected: After 10 seconds the message appears, earlier results stay and the busy state clears; the next search works.  
  Evidence: browser › *a request that never answers times out with a message and can be retried*

- [x] **TC-UI-RES-005 · The workspace stays usable on a slow 3G network and within its page weight**  
  Type: Performance · Priority: P2 · Level: Browser (network emulation)  
  Steps: Emulate 400 ms latency and 400 kbit/s with the cache off; load a workspace; search; open a PR.  
  Expected: Under 512 KiB transferred and loaded within 15 s (measured: about 297 KiB in about 10.5 s); search and navigation still work.  
  Evidence: browser › *the workspace loads within its page weight and stays usable on a slow network*

- [x] **TC-UI-RES-006 · Failed searches keep the records the person had**  
  Type: Resilience · Priority: P2 · Level: Browser  
  Steps: Make the search answer 503; search; retry after recovery.  
  Expected: The error is shown next to the search, the earlier rows and URL stay, and the retry works.  
  Evidence: browser › *failed PR searches preserve records and query bookmarks redirect to dedicated pages*

- [x] **TC-UI-RES-007 · Queue feedback handles pending, duplicate, failed and expired-session requests**  
  Type: Resilience · Priority: P3 · Level: Browser  
  Steps: Submit the developer ping; submit twice; make it fail; expire the session.  
  Expected: A pending state, a duplicate notice, a failure message and a sign-in message respectively; the form is never stuck.  
  Evidence: browser › *queue feedback handles pending duplicate failures and expired session*

- [x] **TC-UI-RES-008 · A session that expires mid-task returns the whole page to sign-in**  
  Type: Resilience · Priority: P2 · Level: Browser  
  Steps: Expire the session, then select a repository panel.  
  Expected: The whole document navigates to sign-in instead of a broken panel.  
  Evidence: browser › *expired sessions navigate through sign-in and return to the requested page*

## 4. Accessibility (TC-UI-A11Y)

- [x] **TC-UI-A11Y-001 · Every page passes the basic accessibility checks**  
  Type: Accessibility · Priority: P1 · Level: Browser  
  Steps: Audit the home page and 12 workspace pages (dashboard, repositories, workspace, two PRs, Secrets, Outbound, logs, Builds, Previews, FAQ, developer tools).  
  Expected: A page language, a title, exactly one `h1`, a `main` landmark, an accessible name on every visible link, button, switch and summary, a label on every field, `alt` on every image, and no duplicate IDs.  
  Evidence: browser › *every page passes basic accessibility checks*

- [x] **TC-UI-A11Y-002 · Keyboard use reaches and leaves every dialog**  
  Type: Accessibility · Priority: P1 · Level: Browser  
  Steps: Tab and Shift+Tab through the sign-out and Automatic dialogs; close the teardown dialog and the command palette with Escape.  
  Expected: Focus stays inside an open dialog and returns to its trigger when it closes.  
  Evidence: browser › *logout modal preserves page scroll and traps and restores focus*; browser › *Automatic confirms before enabling saves in place and live updates respect modal focus*; browser › *command palette filters destinations, navigates and restores focus*

- [x] **TC-UI-A11Y-003 · Reduced motion is respected**  
  Type: Accessibility · Priority: P3 · Level: Browser  
  Steps: Load the home page with reduced motion; scroll the reveals offscreen and back.  
  Expected: Reveals happen once, pause offscreen and do not animate with reduced motion.  
  Evidence: browser › *home reveals once pauses offscreen and respects reduced motion*

## 5. Without JavaScript (TC-UI-NOJS)

- [x] **TC-UI-NOJS-001 · Content and confirmations work without JavaScript**  
  Type: Usability · Priority: P2 · Level: Browser (JavaScript disabled)  
  Steps: Browse and use confirmations with JavaScript off.  
  Expected: All content is readable and native confirmation forms still work.  
  Evidence: browser › *content and native confirmation remain usable without JavaScript*

- [x] **TC-UI-NOJS-002 · Search, pagination and Automatic work without JavaScript**  
  Type: Usability · Priority: P2 · Level: Browser (JavaScript disabled)  
  Steps: Search and page PRs, open one, go back, enable Automatic.  
  Expected: Full-page forms do it all and keep the filters.  
  Evidence: browser › *PR search and pagination work without JavaScript*

- [x] **TC-UI-NOJS-003 · Secrets and logs work without JavaScript**  
  Type: Usability · Priority: P2 · Level: Browser (JavaScript disabled)  
  Steps: Save secrets and pasted `.env` text; page through logs.  
  Expected: Both work through plain forms and links.  
  Evidence: browser › *the secrets editor works without JavaScript and pasted .env text is saved*; browser › *build logs paginate and stay readable without JavaScript*

## 6. Honest states (TC-UI-STATE)

- [x] **TC-UI-STATE-001 · A blocked build is shown disabled with the reason and the fix**  
  Type: Usability · Priority: P1 · Level: Browser + Integration  
  Steps: Open a PR whose worker has no preview domain.  
  Expected: The Build control stays visible but disabled, with the reason and what fixes it.  
  Evidence: browser › *a blocked build stays visible, disabled, with the reason and the fix*; `internal/pullrequests` › `TestBuildIsUnavailableWithoutAWorkerThatCanBuild`

- [x] **TC-UI-STATE-002 · Unavailable repositories and empty states cannot mutate anything**  
  Type: Negative · Priority: P1 · Level: Browser + Integration  
  Steps: Open an unavailable repository's PRs and empty PR lists; post policy, refresh and build there.  
  Expected: No actions are offered, the reason is stated, and forced mutations are refused.  
  Evidence: browser › *unavailable repository PRs offer no actions and say why*; browser › *unavailable repositories and empty PR states remain clear and cannot request mutations*; `internal/pullrequests` › `TestWorkspaceHTTPAuthorizationCSRFActions`

- [x] **TC-UI-STATE-003 · Previews show what is live, Builds show attempts, waits and failures**  
  Type: Positive · Priority: P2 · Level: Browser + Integration  
  Steps: Open both lists with a live preview on an older commit, a preview whose newer build failed, a waiting request, and a first build that failed before going live; click a Previews row outside its title; open `/environments`; tear one down from its row.  
  Expected: Previews lists only what serves a commit (Live, Live · updating, Live · update failed, Removing) with **Open preview** as the row's action; the never-live failure is on Builds only; clicking a Previews row stays on the page; `/environments` moves to `/previews`; waits and failures are explained; teardown confirms per row.  
  Evidence: browser › *Previews show what is live with its link, Builds show attempts, and teardown confirms per row*; `internal/pullrequests` › `TestPreviewsAndBuildsShowRealDeliveryState`, `TestPreviewsListOnlyWhatServes`

- [x] **TC-UI-STATE-004 · The Overview reflects real state only**  
  Type: Positive · Priority: P2 · Level: Browser + Unit  
  Steps: Open an empty and a busy workspace; finish the setup steps.  
  Expected: The pipeline, tabs and rows match the data; the setup checklist follows real progress and goes away when done; the worker note says what fixes building; nothing is invented.  
  Evidence: browser › *Overview places open PRs along the pipeline and its activity tabs and rows are interactive*; `web/pages/dashboard` › `TestSetupChecklistFollowsRealState`; `web/pages/dashboard` › `TestWorkerNoteSaysWhatFixesBuilding`

- [x] **TC-UI-STATE-005 · A preview is built once and its address can be copied**  
  Type: Positive · Priority: P1 · Level: Browser  
  Steps: Build a PR; open a PR with a live preview on an earlier commit; copy its address.  
  Expected: A queued request offers no second build; the address opens in a new tab and is copied; expiry and "earlier commit" are stated.  
  Evidence: browser › *owner builds once and tears a preview down only after confirming in a modal*

- [x] **TC-UI-STATE-006 · Conversations on the home page and FAQ**  
  Type: Usability · Priority: P3 · Level: Browser + Unit  
  Steps: Step through "How it works"; open FAQ answers from the home page, a link and the workspace.  
  Expected: One question at a time with jumps to any stage; every question is answered, filed and linkable.  
  Evidence: browser › *how it works asks one question at a time and jumps to any stage*; browser › *the FAQ answers as a conversation on the home page, from a link and in the workspace*; `web/pages/faq` › `TestEveryQuestionIsAnsweredFiledAndLinkable`

## 7. Untrusted data (TC-UI-SAFE)

- [x] **TC-UI-SAFE-001 · GitHub data renders as text and links only to HTTPS**  
  Type: Security · Priority: P1 · Level: Unit  
  Steps: Render the PR list and detail with a script in the title, an `onerror` image in the author and branch, a failure log with markup, a hostile search, and a `javascript:` GitHub address.  
  Expected: All of it is escaped text; no `<script>`, no `<img onerror>`, no `javascript:` link; a genuine `https://` address still links. *Defect D-02, fixed in this cycle.*  
  Evidence: `web/pages/pullrequests` › `TestGitHubDataRendersAsText`; `web/pages/pullrequests` › `TestCommitURLComesFromThePullRequestAddress`
