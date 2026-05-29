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

})();
