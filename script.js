(function () {
  'use strict';

  /* ── Scroll reveal ──────────────────────────────────────────────── */

  var chapters = document.querySelectorAll('.chapter, .colophon');
  if ('IntersectionObserver' in window && chapters.length) {
    var obs = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) {
          e.target.classList.add('visible');
          obs.unobserve(e.target);
        }
      });
    }, { threshold: 0.06, rootMargin: '0px 0px -32px 0px' });
    [].forEach.call(chapters, function (el) { obs.observe(el); });
  } else {
    [].forEach.call(chapters, function (el) { el.classList.add('visible'); });
  }

  /* ── Diagram border glow on hover ───────────────────────────────── */

  var diagrams = document.querySelectorAll('.diagram');
  [].forEach.call(diagrams, function (el) {
    el.addEventListener('mouseenter', function () {
      el.style.transition = 'border-color 0.5s ease';
      el.style.borderColor = '#3a3450';
    });
    el.addEventListener('mouseleave', function () {
      el.style.borderColor = '';
    });
  });

  /* ── Copy buttons ───────────────────────────────────────────────── */

  var copyBtns = document.querySelectorAll('.cmd-copy');
  if (copyBtns.length && navigator.clipboard) {
    [].forEach.call(copyBtns, function (btn) {
      btn.addEventListener('click', function () {
        var text = this.getAttribute('data-cmd');
        if (!text) return;
        navigator.clipboard.writeText(text).then(function () {
          btn.classList.add('copied');
          var orig = btn.textContent;
          btn.textContent = '\u2713';
          setTimeout(function () {
            btn.textContent = orig;
            btn.classList.remove('copied');
          }, 1200);
        });
      });
    });
  }

  /* ── Cache Busting & Auto Update Check ──────────────────────────── */

  function checkSiteVersion() {
    var versionMeta = document.querySelector('meta[name="version"]');
    if (!versionMeta) return;
    var currentVersion = versionMeta.getAttribute('content');

    // Fetch index.html with a cache-buster query parameter to bypass HTTP caching
    var cacheBuster = new Date().getTime();
    var fetchUrl = window.location.origin + window.location.pathname;
    var urlWithBuster = fetchUrl + (fetchUrl.indexOf('?') >= 0 ? '&' : '?') + '_cb=' + cacheBuster;

    fetch(urlWithBuster, { cache: 'no-store' })
      .then(function (res) {
        if (!res.ok) throw new Error('Failed to fetch the latest site version');
        return res.text();
      })
      .then(function (html) {
        // Parse out the version content from the returned HTML body/meta tags
        var match = html.match(/<meta[^>]+name=["']version["'][^>]+content=["']([^"']+)["']/i) ||
                    html.match(/<meta[^>]+content=["']([^"']+)["'][^>]+name=["']version["']/i);
        if (match && match[1]) {
          var serverVersion = match[1].trim();
          if (serverVersion !== currentVersion) {
            console.log('[Updater] New version detected on server: ' + serverVersion + ' (Local: ' + currentVersion + '). Reloading...');
            // Perform reload to fetch the latest changes
            window.location.reload();
          }
        }
      })
      .catch(function (err) {
        console.warn('[Updater] Update check failed:', err);
      });
  }

  // Check for updates 3 seconds after page load
  setTimeout(checkSiteVersion, 3000);

})();
