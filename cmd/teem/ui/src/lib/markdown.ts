// renderMarkdownSafe parses markdown to HTML via marked, then runs the
// output through DOMPurify so anything injected from a transcript turn,
// leader chat message, plan brief, or task notes can't smuggle a
// <script>, an event handler, or a javascript: URL into the page. The
// daemon already trusts most of these channels, but the transcripts
// surface in particular can include arbitrary worker-produced text, so
// every dangerouslySetInnerHTML site goes through this helper.
//
// Returns the same {__html} shape callers were already passing to
// dangerouslySetInnerHTML, so swapping in is a single-line change at
// each site.

import { marked } from 'marked';
import DOMPurify from 'dompurify';

export function renderMarkdownSafe(text: string | undefined | null): string {
  if (!text) return '';
  let html: string;
  try {
    html = marked.parse(text, { async: false }) as string;
  } catch {
    html = escapeHTML(text);
  }
  return DOMPurify.sanitize(html);
}

function escapeHTML(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
