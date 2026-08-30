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
//   2. The logo itself, embedded rather than linked. It is the same
//      artwork the README shows, carried in the message as a data: URI --
//      about fourteen kilobytes -- so it costs no request and cannot report
//      that the mail was opened. It stands on a plate that stays white in
//      both colour schemes, because the artwork is dark ink and the sheet
//      is not always light; swapping two images by media query is the usual
//      answer and Outlook honours no media query, so it would show both.
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
// The wordmark, as it appears in the README, reduced to the width it is
// shown at and base64'd so the message carries its own artwork.
const LOGO =
  "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";

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

              <!-- The lockup is the product's own logo, embedded rather than
                   linked: a data: URI costs the reader no request and leaks
                   nothing about the mail being opened, which a hosted image
                   would do on both counts.

                   It sits on a plate that stays white in both colour schemes.
                   The artwork is dark ink on nothing, so it would disappear
                   against the dark sheet -- and the usual fix, two images
                   swapped by a media query, shows both of them in Outlook,
                   which honours no media query at all. One image on a plate
                   that never changes is correct everywhere.

                   alt is the product name, so a client that strips data:
                   images (some do) still opens on "r2backup" rather than on
                   a broken-image icon. -->
              <table role="presentation" cellpadding="0" cellspacing="0" border="0" bgcolor="#ffffff"
                     style="background:#ffffff;border-radius:10px;">
                <tr>
                  <td align="left" valign="middle" style="padding:6px 12px;">
                    <img src="data:image/png;base64,${LOGO}" width="190" height="58" alt="r2backup"
                         style="display:block;width:190px;height:58px;border:0;outline:none;text-decoration:none;">
                  </td>
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
