package deliver

// PokeSentinel marks a prompt as pager's own wake poke rather than something a
// person typed.
//
// A poke arrives through the host's message-injection path, so the receiving
// session's UserPromptSubmit hook fires with a non-empty prompt — which is
// indistinguishable, at the hook boundary, from the user pressing enter. That
// matters because a user prompt ends the preceding exchange and resets the
// session's causal epoch. Letting a poke do that would hand the sender control
// of the recipient's causal state, and would drop the recipient's inbound
// pointer so its next reply is billed as human-origin rather than caused.
//
// The sentinel travels in the poke body and hookio checks for it. It lives here
// rather than in the wake package so the hook can recognise a poke without
// depending on the code that sends one.
//
// Spoofing it is possible and uninteresting: suppressing the reset keeps the
// recipient's replies at their real causal depth, which charges them against
// the tighter per-pair budget. Imitating the marker costs the imitator.
const PokeSentinel = "[pager-poke]"
