// The sign-in email.
//
// One person, one job, five seconds: they are part-way through `r2b setup`,
// they have switched to their inbox, and they need six digits back in the
// terminal. Everything here is sized to that. The code is the loudest thing
// on the page, it is selectable as one run of characters so a copy-paste
// picks up nothing else, and the two facts that change what they do next --
// it expires, and it works once -- sit directly under it.
//
// Three constraints shaped the markup, in this order:
//
//   1. No remote requests. No hosted logo, no web font, no tracking pixel,
//      nothing to be blocked and nothing to leak that the mail was opened.
//      This is a tool whose whole argument is that your data is yours, and
//      an email that phones home on open would be arguing the other way. It
//      also removes the failure mode that ruins most branded mail: the
//      version with images off, which for many people is the only version.
//
//   2. The logo, drawn rather than fetched. The mark is a terminal prompt
//      knocked white out of a black hexagon, and the wordmark is a lowercase
//      geometric sans. The prompt is two ASCII characters and reproduces
//      exactly; the hexagon becomes a rounded square, which is as close as a
//      table cell gets and closer than a broken image. There is no web font
//      in an email that Gmail will honour, so the wordmark is set in the
//      nearest geometric grotesque the reader already has.
//
//   3. Tables, inline styles, and no reliance on the <style> block. Outlook
//      renders this with Word's engine: no flexbox, no border-radius, no
//      box-shadow. Everything that matters survives losing all three, and
//      the <style> block carries only what is a bonus when it lands --
//      dark mode, and the narrow-screen sizes.
//
// Colour is the one thing this design does, and it does it once: the code
// block is always the sheet's exact opposite. Black on white, white on
// black -- the same relationship the mark has, and the reason it works in
// both schemes without a second design.

const SANS = `-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'Helvetica Neue',Arial,sans-serif`;
const MONO = `ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,'Liberation Mono',monospace`;

export interface CodeEmail {
  subject: string;
  html: string;
  text: string;
}

/**
 * renderCodeEmail builds the sign-in email for one code.
 *
 * expiresInMinutes is a parameter and not a constant here because the
 * lifetime lives in the auth handler. Copy that says "10 minutes" beside a
 * server that has quietly moved to five is worse than no copy at all.
 */
export function renderCodeEmail(code: string, expiresInMinutes: number): CodeEmail {
  const expiry = `${expiresInMinutes} minute${expiresInMinutes === 1 ? "" : "s"}`;
  return {
    // The code is deliberately not in the subject. It would be convenient,
    // and it would also put it on a lock screen for anyone standing behind
    // you. The person reading this asked for it thirty seconds ago and is
    // already looking.
    subject: "Your r2backup sign-in code",
    html: html(code, expiry),
    text: text(code, expiry),
  };
}

function text(code: string, expiry: string): string {
  // The plain-text part is not a fallback nobody sees. Terminal mail
  // clients, notification previews and screen readers all take this one, and
  // the people running a backup tool from a command line are more likely
  // than most to be reading it.
  return [
    "r2backup",
    "",
    "Enter this code to finish signing in:",
    "",
    `    ${code}`,
    "",
    `It expires in ${expiry}, and works once.`,
    "",
    "If you did not ask for this, ignore it. Nothing changes until the",
    "code is entered, and this address is not added to anything.",
    "",
    "github.com/saurabhhbansal/r2backup",
    "",
  ].join("\n");
}

function html(code: string, expiry: string): string {
  return `<!doctype html>
<html lang="en" style="margin:0;padding:0;">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="supported-color-schemes" content="light dark">
<title>Your r2backup sign-in code</title>
<!--[if mso]>
<style>body,table,td,div,p,a{font-family:'Segoe UI',Arial,sans-serif !important;}</style>
<![endif]-->
<style>
  /* Everything in here is an improvement on a page that is already correct
     without it. Gmail keeps the media queries; Outlook keeps none of it. */
  @media (prefers-color-scheme: dark) {
    .ground { background:#09090b !important; }
    .sheet { background:#111113 !important; border-color:#27272a !important; box-shadow:none !important; }
    .ink { color:#fafafa !important; }
    .ink-2 { color:#d4d4d8 !important; }
    .ink-3 { color:#a1a1aa !important; }
    .rule { border-color:#27272a !important; }
    /* The code block stays the sheet's opposite, so it inverts with it,
       and so does the mark: white prompt out of black becomes black out of
       white, which is the second logo rather than a washed-out first one.
       .mark sets its own colour because the class is on the cell holding
       the prompt -- a descendant selector here left it white on white. */
    .codeblock { background:#fafafa !important; }
    .codeblock td { color:#09090b !important; }
    .mark { background:#fafafa !important; color:#09090b !important; }
  }
  @media only screen and (max-width:520px) {
    .pad { padding:28px 24px 24px 24px !important; }
    .code { font-size:30px !important; letter-spacing:.16em !important; text-indent:.08em !important; }
  }
</style>
</head>
<body class="ground" style="margin:0;padding:0;width:100%;background:#f4f4f5;">
  <!-- Shown in the inbox list and the notification, in place of the first
       words of the body. Without it a client reads down into the markup and
       shows the reader "Enter this code to finish signing in r2backup Your".
       The code is not in here either, for the reason the subject is not. -->
  <div style="display:none;font-size:1px;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;mso-hide:all;">
    Your sign-in code is inside, and it expires in ${escapeHtml(expiry)}.
    &#8199;&#65279;&#847;&#8199;&#65279;&#847;&#8199;&#65279;&#847;&#8199;&#65279;&#847;&#8199;&#65279;&#847;&#8199;&#65279;&#847;&#8199;&#65279;&#847;&#8199;&#65279;&#847;
  </div>

  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="ground" bgcolor="#f4f4f5" style="background:#f4f4f5;">
    <tr>
      <td align="center" style="padding:40px 16px;">

        <table role="presentation" width="480" cellpadding="0" cellspacing="0" border="0" class="sheet" bgcolor="#ffffff"
               style="width:480px;max-width:480px;background:#ffffff;border:1px solid #e4e4e7;border-radius:14px;box-shadow:0 1px 2px rgba(9,9,11,.04),0 10px 28px rgba(9,9,11,.07);">
          <tr>
            <td class="pad" style="padding:36px 40px 32px 40px;">

              <!-- The lockup: the prompt out of the mark, then the wordmark.
                   Two cells rather than an image, so it is the same on every
                   client and in every state. -->
              <table role="presentation" cellpadding="0" cellspacing="0" border="0">
                <tr>
                  <td class="mark" bgcolor="#09090b" width="34" height="34" align="center" valign="middle"
                      style="width:34px;height:34px;background:#09090b;border-radius:9px;color:#ffffff;font-family:${MONO};font-size:15px;line-height:34px;mso-line-height-rule:exactly;">&gt;_</td>
                  <td width="12" style="width:12px;">&nbsp;</td>
                  <td class="ink" valign="middle"
                      style="color:#09090b;font-family:${SANS};font-size:19px;font-weight:700;letter-spacing:-.035em;line-height:34px;mso-line-height-rule:exactly;">r2backup</td>
                </tr>
              </table>

              <!-- The heading instructs rather than labels. "Verification
                   code" tells someone what they are looking at, which they
                   can see; this tells them what to do with it. -->
              <p class="ink" style="margin:30px 0 0 0;padding:0;color:#09090b;font-family:${SANS};font-size:20px;font-weight:600;letter-spacing:-.015em;line-height:1.35;">
                Enter this code to finish signing in.
              </p>

              <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="codeblock" bgcolor="#09090b"
                     style="margin:22px 0 0 0;background:#09090b;border-radius:12px;">
                <tr>
                  <!-- text-indent answers the trailing letter-space that
                       centring counts and nobody can see: without it the
                       digits sit visibly left of centre. -->
                  <td align="center" class="code"
                      style="padding:26px 16px;color:#fafafa;font-family:${MONO};font-size:40px;font-weight:600;letter-spacing:.22em;text-indent:.11em;line-height:1.1;mso-line-height-rule:exactly;">${escapeHtml(code)}</td>
                </tr>
              </table>

              <p class="ink-3" style="margin:14px 0 0 0;padding:0;color:#71717a;font-family:${SANS};font-size:13px;line-height:1.5;">
                It expires in ${escapeHtml(expiry)}, and works once.
              </p>

              <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:28px 0 0 0;">
                <tr><td class="rule" style="border-top:1px solid #e4e4e7;font-size:0;line-height:0;">&nbsp;</td></tr>
              </table>

              <p class="ink-2" style="margin:20px 0 0 0;padding:0;color:#52525b;font-family:${SANS};font-size:13px;line-height:1.6;">
                If you did not ask for this, ignore it. Nothing changes until the code is
                entered, and this address is not added to anything.
              </p>

            </td>
          </tr>
        </table>

        <table role="presentation" width="480" cellpadding="0" cellspacing="0" border="0" style="width:480px;max-width:480px;">
          <tr>
            <td align="center" class="ink-3" style="padding:20px 16px 0 16px;color:#71717a;font-family:${SANS};font-size:12px;line-height:1.6;">
              Sent by r2backup because someone asked to sign in with this address.<br>
              It loads nothing and tracks nothing.<br>
              <a href="https://github.com/saurabhhbansal/r2backup" class="ink-3" style="color:#71717a;text-decoration:underline;text-underline-offset:2px;">github.com/saurabhhbansal/r2backup</a>
            </td>
          </tr>
        </table>

      </td>
    </tr>
  </table>
</body>
</html>`;
}

// escapeHtml is not decoration. The code is generated here and is six digits,
// but it is interpolated into markup that is then sent to an address, and a
// template that only escapes the values it currently needs to is one commit
// away from not escaping the one that matters.
function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}
