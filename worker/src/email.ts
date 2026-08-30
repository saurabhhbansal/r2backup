import { renderCodeEmail } from "./emailTemplate";

// Normalised before it's ever used as a lookup key: "Alice@Foo.com " and
// "alice@foo.com" must be the same account, or a user retyping their email
// slightly differently on a second computer silently lands on a fresh,
// empty vault instead of the one they meant to pull down.
export function normalizeEmail(email: string): string {
  return email.trim().toLowerCase();
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export function isValidEmail(email: string): boolean {
  return email.length > 0 && email.length <= 254 && EMAIL_RE.test(email);
}

export interface Mailer {
  // expiresInMinutes travels with the code so the email cannot say one
  // lifetime while the handler enforces another.
  sendCode(email: string, code: string, expiresInMinutes: number): Promise<void>;
}

export class ResendMailer implements Mailer {
  constructor(private readonly apiKey: string) {}

  async sendCode(email: string, code: string, expiresInMinutes: number): Promise<void> {
    const mail = renderCodeEmail(code, expiresInMinutes);
    // Both parts, every time. The HTML is what most people see; the text
    // part is what terminal clients, notification previews and screen
    // readers take, and a message sent without it is one those readers get
    // as an attachment or as nothing.
    const res = await fetch("https://api.resend.com/emails", {
      method: "POST",
      headers: {
        Authorization: `Bearer ${this.apiKey}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        from: "r2backup <no-reply@flexpod.cc>",
        to: [email],
        subject: mail.subject,
        html: mail.html,
        text: mail.text,
      }),
    });
    if (!res.ok) {
      // No secrets in logs: Resend's error body can echo the request back,
      // and the request body is the one place the plaintext code exists
      // outside the user's inbox. Log only the status.
      console.error(`resend send failed with status ${res.status}`);
      throw new Error("failed to send code email");
    }
  }
}
