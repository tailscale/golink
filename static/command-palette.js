(function () {
  const root = document.querySelector('[data-command-palette]');
  if (!root) {
    return;
  }

  const input = root.querySelector('[data-command-palette-input]');
  const resultsList = root.querySelector('[data-command-palette-results]');
  const loading = root.querySelector('[data-command-palette-loading]');
  const empty = root.querySelector('[data-command-palette-empty]');
  const openButtons = document.querySelectorAll('[data-command-palette-open]');
  const closeButtons = root.querySelectorAll('[data-command-palette-close]');

  let isOpen = false;
  let activeIndex = -1;
  let results = [];
  let controller = null;
  let fetchTimer = null;
  let requestId = 0;

  function setVisible(node, visible) {
    node.classList.toggle('hidden', !visible);
  }

  function openPalette() {
    if (isOpen) {
      input.focus();
      input.select();
      return;
    }
    isOpen = true;
    root.classList.remove('hidden');
    root.setAttribute('aria-hidden', 'false');
    document.body.classList.add('command-palette-open');
    input.focus();
    input.select();
    loadResults(input.value.trim());
  }

  function closePalette() {
    if (!isOpen) {
      return;
    }
    isOpen = false;
    root.classList.add('hidden');
    root.setAttribute('aria-hidden', 'true');
    document.body.classList.remove('command-palette-open');
    if (controller) {
      controller.abort();
      controller = null;
    }
  }

  function renderResults() {
    resultsList.innerHTML = '';
    results.forEach((result, index) => {
      const item = document.createElement('li');
      item.setAttribute('role', 'option');
      item.setAttribute('id', 'command-palette-option-' + index);
      item.setAttribute('aria-selected', index === activeIndex ? 'true' : 'false');

      const link = document.createElement('a');
      link.href = result.path;
      link.className = 'command-palette-result' + (index === activeIndex ? ' is-active' : '');
      link.dataset.index = String(index);

      const titleRow = document.createElement('div');
      titleRow.className = 'command-palette-result-title';
      titleRow.textContent = 'go/' + result.short;
      if (typeof result.numClicks === 'number' && result.numClicks > 0) {
        const badge = document.createElement('span');
        badge.className = 'command-palette-result-badge';
        badge.textContent = result.numClicks + ' click' + (result.numClicks === 1 ? '' : 's');
        titleRow.appendChild(badge);
      }

      const meta = document.createElement('div');
      meta.className = 'command-palette-result-meta';
      meta.textContent = [result.long, result.owner].filter(Boolean).join(' • ');

      link.appendChild(titleRow);
      link.appendChild(meta);
      link.addEventListener('mousemove', function () {
        if (activeIndex !== index) {
          activeIndex = index;
          syncActiveDescendant();
          renderResults();
        }
      });
      item.appendChild(link);
      resultsList.appendChild(item);
    });

    setVisible(empty, !loading.classList.contains('hidden') ? false : results.length === 0);
    syncActiveDescendant();
  }

  function syncActiveDescendant() {
    if (activeIndex >= 0 && activeIndex < results.length) {
      input.setAttribute('aria-activedescendant', 'command-palette-option-' + activeIndex);
    } else {
      input.removeAttribute('aria-activedescendant');
    }
  }

  async function loadResults(query) {
    if (controller) {
      controller.abort();
    }
    controller = new AbortController();
    const currentRequest = ++requestId;
    setVisible(loading, true);
    setVisible(empty, false);

    const search = new URLSearchParams({ format: 'json', limit: '8' });
    if (query) {
      search.set('q', query);
    }

    try {
      const response = await fetch('/.search?' + search.toString(), {
        headers: { Accept: 'application/json' },
        signal: controller.signal,
      });
      if (!response.ok) {
        throw new Error('Search failed');
      }
      const data = await response.json();
      if (currentRequest !== requestId) {
        return;
      }
      results = Array.isArray(data.results) ? data.results : [];
      activeIndex = results.length > 0 ? 0 : -1;
      setVisible(loading, false);
      renderResults();
    } catch (error) {
      if (error && error.name === 'AbortError') {
        return;
      }
      results = [];
      activeIndex = -1;
      setVisible(loading, false);
      setVisible(empty, true);
      resultsList.innerHTML = '';
      syncActiveDescendant();
    }
  }

  function queueSearch() {
    window.clearTimeout(fetchTimer);
    fetchTimer = window.setTimeout(function () {
      loadResults(input.value.trim());
    }, 120);
  }

  function moveSelection(direction) {
    if (!results.length) {
      return;
    }
    activeIndex += direction;
    if (activeIndex < 0) {
      activeIndex = results.length - 1;
    } else if (activeIndex >= results.length) {
      activeIndex = 0;
    }
    renderResults();
    const active = resultsList.querySelector('[data-index="' + activeIndex + '"]');
    if (active) {
      active.scrollIntoView({ block: 'nearest' });
    }
  }

  function activateSelection() {
    if (activeIndex < 0 || activeIndex >= results.length) {
      return;
    }
    window.location.href = results[activeIndex].path;
  }

  openButtons.forEach(function (button) {
    button.addEventListener('click', openPalette);
  });

  closeButtons.forEach(function (button) {
    button.addEventListener('click', closePalette);
  });

  input.addEventListener('input', queueSearch);
  input.addEventListener('keydown', function (event) {
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      moveSelection(1);
      return;
    }
    if (event.key === 'ArrowUp') {
      event.preventDefault();
      moveSelection(-1);
      return;
    }
    if (event.key === 'Enter') {
      event.preventDefault();
      activateSelection();
      return;
    }
    if (event.key === 'Escape') {
      event.preventDefault();
      closePalette();
    }
  });

  root.addEventListener('click', function (event) {
    const link = event.target.closest('.command-palette-result');
    if (!link) {
      return;
    }
    event.preventDefault();
    window.location.href = link.getAttribute('href');
  });

  document.addEventListener('keydown', function (event) {
    const isModifier = event.ctrlKey || event.metaKey;
    if (isModifier && event.key.toLowerCase() === 'k') {
      event.preventDefault();
      openPalette();
      return;
    }
    if (event.key === 'Escape' && isOpen) {
      event.preventDefault();
      closePalette();
    }
  });
})();
