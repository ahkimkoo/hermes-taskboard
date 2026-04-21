// A tiny, defensive Markdown → HTML renderer.
// Handles: fenced code blocks, headings (# / ## / ###), bold, italic, inline
// code, links, images, blockquotes, ordered/unordered lists, GFM-style
// pipe tables, thematic breaks (---), paragraphs. Everything user-supplied
// is HTML-escaped before regex substitution.
//
// This is deliberately not a full CommonMark parser — it only needs to make
// Hermes's streamed assistant text, task descriptions, and the bundled
// operator manual readable.

export function renderMarkdown(src) {
  if (src == null) return '';
  const text = String(src);

  // 1. Extract fenced code blocks first so their contents don't get eaten by
  //    the other regexes.
  const codeBlocks = [];
  let s = text.replace(/```([a-zA-Z0-9_+-]*)\n([\s\S]*?)```/g, (_, lang, body) => {
    codeBlocks.push({ lang, body });
    return '\u0000CODEBLOCK' + (codeBlocks.length - 1) + '\u0000';
  });

  // 2. HTML-escape the rest.
  s = escapeHTML(s);

  // 3. Headings.
  s = s.replace(/^###\s+(.+)$/gm, '<h3>$1</h3>');
  s = s.replace(/^##\s+(.+)$/gm, '<h2>$1</h2>');
  s = s.replace(/^#\s+(.+)$/gm, '<h1>$1</h1>');

  // 4. Thematic break (horizontal rule): a line with just --- or *** or ___.
  s = s.replace(/^(?:---|\*\*\*|___)\s*$/gm, '<hr>');

  // 5. Blockquotes.
  s = s.replace(/(^|\n)&gt;\s?(.+)/g, (_, nl, body) => nl + '<blockquote>' + body + '</blockquote>');

  // 6. Images (must come before links).
  s = s.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, (_, alt, href) => {
    return '<img src="' + escapeAttr(href) + '" alt="' + escapeAttr(alt) + '">';
  });

  // 7. Links.
  s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, label, href) => {
    return '<a href="' + escapeAttr(href) + '" target="_blank" rel="noopener">' + label + '</a>';
  });

  // 8. Bold / italic / inline code.
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/(^|[^*])\*([^*]+)\*/g, (_, pre, body) => pre + '<em>' + body + '</em>');
  s = s.replace(/`([^`]+)`/g, '<code>$1</code>');

  // 9. GFM pipe tables. Match a header row `| a | b |` followed by a
  //    separator row `|---|---|` (with optional :---: alignment markers)
  //    followed by any number of body rows. Two+ columns required — a
  //    single pipe on a line is almost always meant as a literal char.
  s = s.replace(/(^|\n)(\|[^\n]+\|)\n(\|[\s:|-]+\|)\n((?:\|[^\n]+\|\n?)+)/g,
    (_, lead, header, sep, body) => {
      const splitRow = (row) => row.trim().replace(/^\||\|$/g, '').split(/\s*\|\s*/);
      const aligns = splitRow(sep).map((cell) => {
        const left = cell.startsWith(':');
        const right = cell.endsWith(':');
        if (left && right) return 'center';
        if (right) return 'right';
        if (left) return 'left';
        return '';
      });
      const renderCell = (tag, cell, i) => {
        const a = aligns[i] ? ' style="text-align:' + aligns[i] + '"' : '';
        return '<' + tag + a + '>' + cell + '</' + tag + '>';
      };
      const thead = '<thead><tr>' + splitRow(header).map((c, i) => renderCell('th', c, i)).join('') + '</tr></thead>';
      const rows = body.trim().split('\n').filter(Boolean).map((r) => {
        return '<tr>' + splitRow(r).map((c, i) => renderCell('td', c, i)).join('') + '</tr>';
      }).join('');
      return lead + '<table>' + thead + '<tbody>' + rows + '</tbody></table>';
    });

  // 10. Lists. Group consecutive lines starting with "- " or "* " or "1. ".
  s = s.replace(/(?:^|\n)((?:[-*]\s.+(?:\n|$))+)/g, (m, block) => {
    const items = block.trim().split(/\n/).map((line) => line.replace(/^[-*]\s+/, '').trim());
    return '\n<ul>' + items.map((it) => '<li>' + it + '</li>').join('') + '</ul>';
  });
  s = s.replace(/(?:^|\n)((?:\d+\.\s.+(?:\n|$))+)/g, (m, block) => {
    const items = block.trim().split(/\n/).map((line) => line.replace(/^\d+\.\s+/, '').trim());
    return '\n<ol>' + items.map((it) => '<li>' + it + '</li>').join('') + '</ol>';
  });

  // 11. Paragraphs: double newline as separator. Preserve single newlines as <br>.
  s = s
    .split(/\n{2,}/)
    .map((para) => {
      // Don't wrap block-level tags in <p>.
      if (/^\s*<(h\d|ul|ol|blockquote|pre|img|hr|table)/.test(para)) return para;
      if (/^\s*$/.test(para)) return '';
      return '<p>' + para.replace(/\n/g, '<br>') + '</p>';
    })
    .join('\n');

  // 12. Re-insert code blocks as <pre><code>.
  s = s.replace(/\u0000CODEBLOCK(\d+)\u0000/g, (_, i) => {
    const { lang, body } = codeBlocks[Number(i)];
    return '<pre><code class="lang-' + escapeAttr(lang || '') + '">' + escapeHTML(body) + '</code></pre>';
  });

  return s;
}

function escapeHTML(s) {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}
function escapeAttr(s) {
  return escapeHTML(s).replace(/"/g, '&quot;');
}
