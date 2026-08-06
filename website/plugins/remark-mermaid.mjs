import { visit } from 'unist-util-visit';

// remarkMermaid rewrites ```mermaid fenced code blocks into raw
// `<pre class="mermaid">` HTML nodes at the markdown (mdast) stage — before
// Starlight's Expressive Code (a rehype plugin) processes code blocks, so it
// never turns the diagram source into a styled code block. A small client
// script (see src/components/Head.astro) then renders these with mermaid.js.
//
// The diagram source is HTML-escaped: the browser un-escapes it back to the
// original text via `textContent`, which is what mermaid.run() reads.
export function remarkMermaid() {
  return (tree) => {
    visit(tree, 'code', (node, index, parent) => {
      if (node.lang !== 'mermaid' || !parent || index === null) return;
      parent.children[index] = {
        type: 'html',
        // tabindex="0": the diagram now scrolls horizontally (see the
        // pre.mermaid rule in src/components/Head.astro), and a scrollable
        // region a keyboard cannot reach is unreachable content.
        value: `<pre class="mermaid" role="img" aria-label="diagram" tabindex="0">${escapeHtml(node.value)}</pre>`,
      };
    });
  };
}

function escapeHtml(s) {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}
