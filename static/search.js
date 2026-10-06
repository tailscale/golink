// golink autocomplete combobox.
//
// Progressive enhancement: the <form action="/.search"> in base.html works
// without JS (server renders an HTML results page). This script hijacks
// keystrokes on the input and renders a WAI-ARIA combobox with a listbox
// popup, following the APG pattern at
// https://www.w3.org/WAI/ARIA/apg/patterns/combobox/examples/combobox-autocomplete-list/
//
// Keyboard model:
//   - Printable chars: debounced fetch of /.search?q=... ; popup opens
//   - ArrowDown/ArrowUp: move active option (wraps; includes "Full results"
//     as the last selectable option)
//   - Home/End: first/last option
//   - Enter: activate the currently active option (navigate to its target
//     or, for the "more results" option, to the /.search page)
//   - Cmd/Ctrl/middle-click + Enter/click: open target in a new tab
//   - Shift + Enter/click: open target in a new window
//   - Alt + Enter/click: jump to the link-details page (/.detail/{short})
//     rather than following the link
//   - Escape: close popup, keep input focus and value
//   - Tab: close popup, allow normal focus movement
//
// Per APG, DOM focus stays on the input at all times; the "selected"
// option is tracked via aria-activedescendant.

(function () {
  "use strict";

  var input = document.getElementById("gl-search-input");
  var listbox = document.getElementById("gl-search-listbox");
  var live = document.getElementById("gl-search-live");
  var form = input && input.form;
  if (!input || !listbox || !form) return;

  var DEBOUNCE_MS = 80;
  var TIP_ITEMS = [
    {
      title: "Type to search short links",
      text: "Start typing any short name to see fuzzy matches.",
    },
    {
      title: "Prefix with ? to search destinations",
      text: "Use <code>?docs</code> to also match destination URL bodies.",
    },
    {
      title: "Keyboard shortcuts",
      text: "Use <code>↑</code>/<code>↓</code> to move and <code>Enter</code> to open the selected result.",
    },
    {
      title: "More actions",
      text: "Use <code>Alt+Enter</code> for details, <code>Cmd/Ctrl+Enter</code> for a new tab, and <code>Shift+Enter</code> for a new window.",
    },
  ];
  // Sentinel activeIndex value for the "Full results" footer option.
  // It lives conceptually at results.length; we encode that explicitly
  // so results.length === 0 with an active footer also works.
  var MORE_INDEX = -2;
  var state = {
    query: "",         // the raw query string the current results are for
    results: [],       // latest SearchResult[] from server
    hasMore: false,    // whether a "full results" option is present
    showTips: false,   // whether the empty-state tips popup is showing
    activeIndex: -1,   // 0..results.length-1 for a result,
                       // MORE_INDEX for the "full results" footer,
                       // -1 for nothing active
    debounceTimer: null,
    controller: null,  // AbortController for in-flight fetch
    lastAnnouncedCount: null,
  };

  // Escape text for safe insertion into HTML. We intentionally build
  // the listbox with innerHTML so we can wrap matched characters in
  // <mark> elements; everything user-supplied is run through this first.
  function esc(s) {
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function tipsHTML() {
    var html = "";
    for (var i = 0; i < TIP_ITEMS.length; i++) {
      html +=
        '<li class="gl-search-tip" role="presentation">' +
          '<span class="gl-search-tip-title">' + esc(TIP_ITEMS[i].title) + '</span>' +
          '<span class="gl-search-tip-text">' + TIP_ITEMS[i].text + '</span>' +
        '</li>';
    }
    return html;
  }

  // Wrap matched rune positions in <mark>. Positions are into the rune
  // sequence of `str`; we iterate code points so that multi-byte chars
  // don't break alignment.
  function highlight(str, positions) {
    if (!positions || !positions.length) return esc(str);
    var posSet = new Set(positions);
    var out = "";
    // Use Array.from to iterate by code point.
    var codePoints = Array.from(str);
    for (var p = 0; p < codePoints.length; p++) {
      var ch = esc(codePoints[p]);
      if (posSet.has(p)) {
        out += "<mark>" + ch + "</mark>";
      } else {
        out += ch;
      }
    }
    return out;
  }

  // Position arrays already come split per-field from the server.
  function splitPositions(result) {
    return {
      short: result.shortMatchedIndexes || [],
      long: result.longMatchedIndexes || [],
    };
  }

  // parseQuery mirrors the server-side search input parsing enough for the
  // client to tell whether the user is editing the short name or only the
  // path/query suffix after a '/'.
  function parseQuery(raw) {
    var s = String(raw || "").replace(/^[ \t]+/, "");
    var hasNonLeadingQuestion = s.indexOf("?") > 0;
    if (s.charAt(0) === "?") {
      s = s.slice(1);
    }
    var qmark = s.indexOf("?");
    if (qmark >= 0) {
      s = s.slice(0, qmark);
    }
    var slash = s.indexOf("/");
    if (slash >= 0) {
      return {
        shortQuery: s.slice(0, slash),
        hasSlash: true,
        hasNonLeadingQuestion: hasNonLeadingQuestion,
      };
    }
    return {
      shortQuery: s,
      hasSlash: false,
      hasNonLeadingQuestion: hasNonLeadingQuestion,
    };
  }

  function activeResultShort() {
    if (state.activeIndex < 0) return "";
    var r = state.results[state.activeIndex];
    return r ? r.short : "";
  }

  // preserveSelectionAfterSlash reports whether a selected result should stay
  // selected across a suggestions refresh. This is only true when the user is
  // editing after (or has just typed) a '/' or a non-leading '?' and the
  // short-query portion has not changed, meaning only the link template/path
  // preview is changing.
  function preserveSelectionAfterSlash(prevRaw, nextRaw) {
    var prev = parseQuery(prevRaw);
    var next = parseQuery(nextRaw);
    return (next.hasSlash || next.hasNonLeadingQuestion) &&
      next.shortQuery !== "" &&
      prev.shortQuery === next.shortQuery;
  }

  // navigate handles opening `target` according to the modifier keys held
  // when the event fired. Mirrors standard browser link behavior:
  //   - Cmd (macOS), Ctrl (other), or middle-click: new background tab
  //   - Shift: new window
  //   - Otherwise: current tab
  // The second parameter is the original event (KeyboardEvent or
  // MouseEvent); falsy values default to current-tab navigation.
  function navigate(target, ev) {
    if (!target) return;
    var meta = !!(ev && (ev.metaKey || ev.ctrlKey));
    var middle = !!(ev && ev.type === "mouseup" && ev.button === 1);
    var shift = !!(ev && ev.shiftKey);
    if (meta || middle) {
      window.open(target, "_blank", "noopener,noreferrer");
      return;
    }
    if (shift) {
      window.open(
        target,
        "_blank",
        "noopener,noreferrer,width=" +
          (window.screen && window.screen.availWidth ? window.screen.availWidth : 1024) +
          ",height=" +
          (window.screen && window.screen.availHeight ? window.screen.availHeight : 768),
      );
      return;
    }
    window.location.href = target;
  }

  // Resolve a "selectable option index" to {kind, target}.
  //   kind "result": resolve against state.results[idx], honoring Alt
  //     which swaps the navigation target for the detail page.
  //   kind "more":   the /.search page.
  // If ev has Alt held and the option is a result, DetailTarget is
  // returned; otherwise the regular Target.
  function targetForIndex(idx, ev) {
    if (idx === MORE_INDEX) {
      if (!state.query) return null;
      return "/.search?q=" + encodeURIComponent(state.query);
    }
    var r = state.results[idx];
    if (!r) return null;
    var alt = !!(ev && ev.altKey);
    return alt && r.detailTarget ? r.detailTarget : r.target;
  }

  // activatable returns the list of activeIndex values the user can
  // currently navigate to, in display order. Used for ArrowUp/Down/Home/End.
  function activatable() {
    var list = [];
    for (var i = 0; i < state.results.length; i++) list.push(i);
    if (state.hasMore) list.push(MORE_INDEX);
    return list;
  }

  // domIdForIndex returns the DOM id for the option at the given virtual
  // index (so aria-activedescendant can point at it).
  function domIdForIndex(idx) {
    if (idx === MORE_INDEX) return "gl-search-more";
    if (idx < 0) return "";
    return "gl-search-opt-" + idx;
  }

  function render() {
    if (state.showTips) {
      listbox.innerHTML = tipsHTML();
      listbox.hidden = false;
      input.setAttribute("aria-expanded", "true");
      input.setAttribute("aria-activedescendant", "");
      return;
    }
    if (!state.results.length && !state.hasMore) {
      listbox.innerHTML = "";
      listbox.hidden = true;
      input.setAttribute("aria-expanded", "false");
      input.setAttribute("aria-activedescendant", "");
      return;
    }
    var html = "";
    for (var i = 0; i < state.results.length; i++) {
      var r = state.results[i];
      var isActive = i === state.activeIndex;
      var optionId = "gl-search-opt-" + i;
      var positions = splitPositions(r);
      var shortHtml = highlight(r.short, positions.short);
      var renderedHtml = positions.long.length
        ? highlight(r.rendered, positions.long)
        : esc(r.rendered);
      html +=
        '<li role="option" id="' + optionId + '"' +
        ' data-kind="result" data-index="' + i + '"' +
        ' data-target="' + esc(r.target) + '"' +
        ' data-detail-target="' + esc(r.detailTarget || "") + '"' +
        ' aria-selected="' + (isActive ? "true" : "false") + '"' +
        ' class="gl-search-option' + (isActive ? " is-active" : "") + '">' +
          '<span class="gl-search-short">go/' + shortHtml + '</span>' +
          '<span class="gl-search-rendered">&rarr; ' + renderedHtml + '</span>' +
        '</li>';
    }
  // "Full results" footer, modelled as a role="option" so it's
    // reachable via ArrowUp/ArrowDown. Alt modifier doesn't apply to
    // this option (there's no detail page for a query).
    if (state.hasMore) {
      var moreHref = "/.search?q=" + encodeURIComponent(state.query);
      var moreActive = state.activeIndex === MORE_INDEX;
      html +=
        '<li role="option" id="gl-search-more"' +
        ' data-kind="more"' +
        ' data-target="' + esc(moreHref) + '"' +
        ' aria-selected="' + (moreActive ? "true" : "false") + '"' +
        ' class="gl-search-footer gl-search-option' +
          (moreActive ? " is-active" : "") + '">' +
          '<span class="gl-search-more">Full results</span>' +
        '</li>';
    }
    listbox.innerHTML = html;
    listbox.hidden = false;
    input.setAttribute("aria-expanded", "true");
    input.setAttribute("aria-activedescendant", domIdForIndex(state.activeIndex));
  }

  function hidePopup() {
    listbox.hidden = true;
    input.setAttribute("aria-expanded", "false");
    input.setAttribute("aria-activedescendant", "");
  }

  function announce(count) {
    if (count === state.lastAnnouncedCount) return;
    state.lastAnnouncedCount = count;
    if (count === 0) {
      live.textContent = "No suggestions.";
    } else if (count === 1) {
      live.textContent = "1 suggestion available.";
    } else {
      live.textContent = count + " suggestions available.";
    }
  }

  function close() {
    state.query = "";
    state.results = [];
    state.hasMore = false;
    state.showTips = false;
    state.activeIndex = -1;
    state.lastAnnouncedCount = null;
    if (state.controller) {
      state.controller.abort();
      state.controller = null;
    }
    render();
  }

  function showTips() {
    if (state.controller) {
      state.controller.abort();
      state.controller = null;
    }
    state.query = "";
    state.results = [];
    state.hasMore = false;
    state.showTips = true;
    state.activeIndex = -1;
    render();
    live.textContent = "Search tips shown.";
  }

  // setActiveByPosition moves to the nth activatable option (wrapping
  // both ends). Position can be negative (for wrap-up) or >= length
  // (for wrap-down).
  function setActiveByPosition(pos) {
    var list = activatable();
    if (!list.length) {
      state.activeIndex = -1;
      return;
    }
    var n = list.length;
    var wrapped = ((pos % n) + n) % n;
    state.activeIndex = list[wrapped];
    render();
    var el = document.getElementById(domIdForIndex(state.activeIndex));
    if (el && el.scrollIntoView) {
      el.scrollIntoView({ block: "nearest" });
    }
  }

  // moveActive shifts the active option by +1 or -1, wrapping.
  function moveActive(delta) {
    var list = activatable();
    if (!list.length) return;
    var curPos = list.indexOf(state.activeIndex);
    if (curPos === -1) {
      // Nothing active yet -> start at either end depending on direction.
      setActiveByPosition(delta > 0 ? 0 : list.length - 1);
      return;
    }
    setActiveByPosition(curPos + delta);
  }

  function fetchResults(q) {
    if (state.controller) state.controller.abort();
    if (!q.trim()) {
      showTips();
      return;
    }
    state.showTips = false;
    var preserveShort = preserveSelectionAfterSlash(state.query, q)
      ? activeResultShort()
      : "";
    var ctrl = new AbortController();
    state.controller = ctrl;
    var url = "/.search?q=" + encodeURIComponent(q);
    fetch(url, {
      headers: { Accept: "application/json" },
      signal: ctrl.signal,
      credentials: "same-origin",
    })
      .then(function (resp) {
        if (!resp.ok) throw new Error("search HTTP " + resp.status);
        return resp.json();
      })
      .then(function (data) {
        // Ignore stale responses (input has changed since this fetch
        // started). We already abort on new input, but guard anyway.
        if (ctrl !== state.controller) return;
        state.query = q;
        state.results = Array.isArray(data.results) ? data.results : [];
        state.showTips = false;
        // Surface "Full results" whenever the server indicates there
        // were more matches beyond the returned set. Also show it
        // unconditionally when there's at least one result, so users
        // have a keyboard-reachable path to the full search page even
        // if they happened to land on a query that returned < 8 hits.
        state.hasMore = !!data.limitHit || state.results.length > 0;
        if (preserveShort) {
          var preservedIdx = -1;
          for (var i = 0; i < state.results.length; i++) {
            if (state.results[i].short === preserveShort) {
              preservedIdx = i;
              break;
            }
          }
          if (preservedIdx >= 0) {
            state.activeIndex = preservedIdx;
          } else {
            state.activeIndex = state.results.length
              ? 0
              : state.hasMore ? MORE_INDEX : -1;
          }
        } else {
          // Pre-activate the first result so Enter navigates to it.
          state.activeIndex = state.results.length
            ? 0
            : state.hasMore ? MORE_INDEX : -1;
        }
        render();
        announce(state.results.length);
      })
      .catch(function (err) {
        if (err && err.name === "AbortError") return;
        // On error, just close the popup; don't spam the user.
        // The no-JS form fallback (Enter submits to /.search) still works.
        close();
      });
  }

  input.addEventListener("input", function () {
    var q = input.value;
    clearTimeout(state.debounceTimer);
    state.debounceTimer = setTimeout(function () {
      fetchResults(q);
    }, DEBOUNCE_MS);
  });

  input.addEventListener("keydown", function (e) {
    switch (e.key) {
      case "ArrowDown":
        if (listbox.hidden) {
          e.preventDefault();
          if (state.results.length || state.hasMore || state.showTips) {
            render();
          } else if (input.value.trim()) {
            fetchResults(input.value);
          }
          if (!state.showTips && state.activeIndex === -1) {
            setActiveByPosition(0);
          }
        } else if (!state.showTips) {
          e.preventDefault();
          moveActive(1);
        }
        break;
      case "ArrowUp":
        if (!listbox.hidden && !state.showTips) {
          e.preventDefault();
          moveActive(-1);
        }
        break;
      case "Home":
        if (!listbox.hidden && !state.showTips) {
          e.preventDefault();
          setActiveByPosition(0);
        }
        break;
      case "End":
        if (!listbox.hidden && !state.showTips) {
          e.preventDefault();
          setActiveByPosition(activatable().length - 1);
        }
        break;
      case "Escape":
        if (!listbox.hidden) {
          e.preventDefault();
          hidePopup();
        }
        break;
      case "Tab":
        if (!listbox.hidden) close();
        break;
      case "Enter":
        // If an option is active, navigate to its target (honoring
        // modifier keys). Otherwise fall through to the form's default
        // submit handler, which lands on /.search for a full search.
        if (state.activeIndex !== -1) {
          var target = targetForIndex(state.activeIndex, e);
          if (target) {
            e.preventDefault();
            navigate(target, e);
          }
        }
        break;
    }
  });

  // Clicking an option navigates to its target. Honor Cmd/Ctrl/Shift/Alt
  // and middle-click.
  listbox.addEventListener("click", function (e) {
    var li = e.target.closest("li[role=option]");
    if (!li) return;
    e.preventDefault();
    var kind = li.getAttribute("data-kind");
    var target;
    if (kind === "more") {
      target = li.getAttribute("data-target");
    } else {
      var alt = !!e.altKey;
      var detail = li.getAttribute("data-detail-target");
      target = alt && detail ? detail : li.getAttribute("data-target");
    }
    navigate(target, e);
  });

  // Middle-click (button 1) doesn't reliably fire "click"; use "auxclick".
  listbox.addEventListener("auxclick", function (e) {
    if (e.button !== 1) return;
    var li = e.target.closest("li[role=option]");
    if (!li) return;
    e.preventDefault();
    var kind = li.getAttribute("data-kind");
    var target = kind === "more"
      ? li.getAttribute("data-target")
      : (e.altKey
          ? (li.getAttribute("data-detail-target") || li.getAttribute("data-target"))
          : li.getAttribute("data-target"));
    if (target) window.open(target, "_blank", "noopener,noreferrer");
  });

  // Hovering an option activates it (mouse + keyboard parity).
  listbox.addEventListener("mousemove", function (e) {
    if (state.showTips) return;
    var li = e.target.closest("li[role=option]");
    if (!li) return;
    var kind = li.getAttribute("data-kind");
    var newIdx;
    if (kind === "more") {
      newIdx = MORE_INDEX;
    } else {
      var i = parseInt(li.getAttribute("data-index"), 10);
      if (Number.isNaN(i)) return;
      newIdx = i;
    }
    if (newIdx !== state.activeIndex) {
      state.activeIndex = newIdx;
      // Update visual/ARIA state without re-scrolling.
      var opts = listbox.querySelectorAll("li[role=option]");
      for (var o = 0; o < opts.length; o++) {
        var thisIdx = opts[o].getAttribute("data-kind") === "more"
          ? MORE_INDEX
          : parseInt(opts[o].getAttribute("data-index"), 10);
        var active = thisIdx === newIdx;
        opts[o].setAttribute("aria-selected", active ? "true" : "false");
        opts[o].classList.toggle("is-active", active);
      }
      input.setAttribute("aria-activedescendant", domIdForIndex(newIdx));
    }
  });

  // Close when clicking outside the form.
  document.addEventListener("mousedown", function (e) {
    if (!form.contains(e.target)) close();
  });

  // Re-open popup on focus if we have a current query with cached results.
  input.addEventListener("focus", function () {
    if (!input.value.trim()) {
      showTips();
      return;
    }
    if (state.results.length || state.hasMore) render();
  });
})();
