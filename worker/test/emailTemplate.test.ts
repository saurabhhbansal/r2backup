import { describe, it, expect } from "vitest";
import { renderCodeEmail } from "../src/emailTemplate";

describe("the sign-in email", () => {
  it("carries the code in both parts", () => {
    const mail = renderCodeEmail("482913", 10);
    expect(mail.html).toContain("482913");
    expect(mail.text).toContain("482913");
  });

  // The lifetime is enforced by the auth handler and stated by the email.
  // Hard-coding it here is how the two drift, and a message that promises
  // ten minutes on a five-minute code is a support request.
  it("states the lifetime it is given, not one of its own", () => {
    expect(renderCodeEmail("111111", 5).html).toContain("5 minutes");
    expect(renderCodeEmail("111111", 5).text).toContain("5 minutes");
    expect(renderCodeEmail("111111", 10).html).toContain("10 minutes");
    expect(renderCodeEmail("111111", 1).html).toContain("1 minute,");
  });

  // Convenient, and it would put the code on a lock screen for anyone
  // standing behind you. The person reading it asked thirty seconds ago.
  it("keeps the code out of the subject and the preview line", () => {
    const mail = renderCodeEmail("482913", 10);
    expect(mail.subject).not.toContain("482913");

    const preheader = mail.html.slice(mail.html.indexOf("mso-hide:all"));
    const end = preheader.indexOf("</div>");
    expect(preheader.slice(0, end)).not.toContain("482913");
  });

  // The whole design rests on this: no hosted logo, no web font, no
  // tracking pixel. It is what makes the mail render identically with
  // images off, and it is the only honest position for a tool that argues
  // your data is yours.
  it("asks the reader's mail client to fetch nothing", () => {
    const html = renderCodeEmail("482913", 10).html;

    // Images are allowed; images the reader has to go and get are not. The
    // logo is carried in the message as a data: URI, so every src here must
    // be one -- an http(s) src would cost a request and would report that
    // the mail had been opened, which is the whole thing this guards.
    const srcs = [...html.matchAll(/<img\b[^>]*\ssrc="([^"]*)"/gi)].map((m) => m[1]);
    expect(srcs.length).toBe(1);
    for (const src of srcs) expect(src.startsWith("data:image/")).toBe(true);
    // And it must still say something useful with images stripped entirely.
    expect(html).toMatch(/<img\b[^>]*\salt="r2backup"/i);

    expect(html).not.toMatch(/background-image/i);
    expect(html).not.toMatch(/@font-face/i);
    expect(html).not.toMatch(/@import/i);
    expect(html).not.toMatch(/src\s*=\s*["']?https?:/i);
    expect(html).not.toMatch(/url\(/i);

    // One link, to the repository, and it goes where it says it goes.
    const hrefs = [...html.matchAll(/href="([^"]+)"/g)].map((m) => m[1]);
    expect(hrefs).toEqual(["https://github.com/saurabhhbansal/r2backup"]);
  });

  // Outlook renders this with Word's engine, which has no flexbox, no
  // border-radius and no box-shadow, and Gmail drops the <style> block's
  // rules on anything it does not recognise. What survives all of that is
  // tables and inline styles, so the layout has to be made of those.
  it("does not depend on the stylesheet or on modern layout", () => {
    const html = renderCodeEmail("482913", 10).html;
    const withoutStyleBlock = html.replace(/<style>[\s\S]*?<\/style>/g, "");
    expect(withoutStyleBlock).toContain("482913");
    expect(withoutStyleBlock).toContain("Enter this code");
    expect(withoutStyleBlock).not.toMatch(/display\s*:\s*(flex|grid)/i);

    // Every colour a reader depends on is inline, so losing the block
    // costs the dark scheme and nothing else.
    expect(html).toContain('bgcolor="#09090b"');
    expect(html).toContain('bgcolor="#ffffff"');
  });

  it("has a dark scheme that inverts the code block with the sheet", () => {
    const html = renderCodeEmail("482913", 10).html;
    expect(html).toContain('name="color-scheme"');
    expect(html).toContain("prefers-color-scheme: dark");
    // The code block is the sheet's opposite in both schemes: dark on
    // light, light on dark. That single rule is what lets one design work
    // in both without a second one.
    const dark = html.slice(html.indexOf("prefers-color-scheme: dark"));
    expect(dark).toMatch(/\.sheet\s*{[^}]*background:#111113/);
    expect(dark).toMatch(/\.codeblock\s*{[^}]*background:#fafafa/);
  });

  it("escapes what it interpolates", () => {
    const mail = renderCodeEmail('<script>"x"</script>', 10);
    expect(mail.html).not.toContain("<script>");
    expect(mail.html).toContain("&lt;script&gt;");
  });

  it("is a complete document a client will not have to guess at", () => {
    const html = renderCodeEmail("482913", 10).html;
    expect(html.startsWith("<!doctype html>")).toBe(true);
    expect(html).toContain('<meta charset="utf-8">');
    expect(html).toContain('<meta name="viewport"');
    expect(html.trimEnd().endsWith("</html>")).toBe(true);
  });

  // Terminal mail clients, notification previews and screen readers all
  // take this part, and the people running a backup tool from a command
  // line are more likely than most to be reading it.
  it("ships a plain-text part that stands on its own", () => {
    const text = renderCodeEmail("482913", 10).text;
    expect(text).not.toContain("<");
    expect(text).toContain("482913");
    expect(text).toContain("works once");
    expect(text).toContain("If you did not ask for this");
  });
});
