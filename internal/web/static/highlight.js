/* Daywatch syntax highlighting — tiny, self-contained (no CDN).
 *
 * Elements opt in with data-hl="sql|php|json"; their text content is
 * re-rendered with token <span>s. dwHighlight() is idempotent and called
 * on load and after every live-reload swap.
 */
(function () {
  const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

  const SQL_KW = 'select|insert|update|delete|from|where|join|left|right|inner|outer|cross|lateral|on|and|or|not|in|is|null|as|order|by|group|having|limit|offset|distinct|count|sum|avg|min|max|like|ilike|between|exists|union|all|case|when|then|else|end|set|values|into|returning|begin|commit|rollback|create|table|index|drop|alter|add|column|primary|key|foreign|references|default|asc|desc|coalesce|nullif|cast|with|using|filter|within|over|partition|interval|true|false';

  const PHP_KW = 'abstract|array|as|break|callable|case|catch|class|clone|const|continue|declare|default|do|echo|else|elseif|empty|enum|extends|final|finally|fn|for|foreach|function|global|if|implements|include|include_once|instanceof|insteadof|interface|isset|list|match|namespace|new|print|private|protected|public|readonly|require|require_once|return|static|switch|throw|trait|try|unset|use|var|while|yield|true|false|null|int|float|string|bool|void|mixed|self|parent|this';

  // Rule order matters: comments and strings must win over keywords.
  const LANGS = {
    sql: {
      flags: 'gi',
      rules: [
        ['c', /--[^\n]*|\/\*[\s\S]*?\*\//],
        ['s', /'(?:[^'\\]|\\.|'')*'?/],
        ['n', /\b\d+(?:\.\d+)?\b/],
        ['p', /\?|\$\d+|:\w+/],
        ['k', new RegExp('\\b(?:' + SQL_KW + ')\\b')],
      ],
    },
    php: {
      flags: 'g',
      rules: [
        ['c', /\/\/[^\n]*|#[^\n]*|\/\*[\s\S]*?\*\//],
        ['s', /'(?:[^'\\]|\\.)*'?|"(?:[^"\\]|\\.)*"?/],
        ['v', /\$\w+/],
        ['n', /\b\d+(?:\.\d+)?\b|\b0x[0-9a-fA-F]+\b/],
        ['k', new RegExp('\\b(?:' + PHP_KW + ')\\b')],
        ['f', /\b[A-Z][A-Za-z0-9_]*(?:\\[A-Z][A-Za-z0-9_]*)*\b/], // class names
      ],
    },
    json: {
      flags: 'g',
      rules: [
        ['key', /"(?:[^"\\]|\\.)*"(?=\s*:)/],
        ['s', /"(?:[^"\\]|\\.)*"/],
        ['n', /-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b/],
        ['k', /\b(?:true|false|null)\b/],
      ],
    },
  };

  const compiled = {};
  function lang(name) {
    if (compiled[name] !== undefined) return compiled[name];
    const def = LANGS[name];
    if (!def) return (compiled[name] = null);
    return (compiled[name] = {
      classes: def.rules.map((r) => r[0]),
      re: new RegExp(def.rules.map((r) => '(' + r[1].source + ')').join('|'), def.flags),
    });
  }

  function highlight(src, name) {
    const l = lang(name);
    if (!l) return esc(src);
    let out = '';
    let last = 0;
    l.re.lastIndex = 0;
    let m;
    while ((m = l.re.exec(src)) !== null) {
      out += esc(src.slice(last, m.index));
      let cls = '';
      for (let i = 0; i < l.classes.length; i++) {
        if (m[i + 1] !== undefined) { cls = l.classes[i]; break; }
      }
      out += '<span class="tok-' + cls + '">' + esc(m[0]) + '</span>';
      last = m.index + m[0].length;
      if (m[0].length === 0) l.re.lastIndex++; // safety against empty matches
    }
    return out + esc(src.slice(last));
  }

  window.dwHighlight = function (root) {
    (root || document).querySelectorAll('[data-hl]').forEach((el) => {
      if (el.dataset.hlDone) return;
      el.dataset.hlDone = '1';
      el.innerHTML = highlight(el.textContent, el.dataset.hl);
    });
  };

  document.addEventListener('DOMContentLoaded', function () { dwHighlight(); });
})();
