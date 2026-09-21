import { useState } from 'react'

/**
 * A collapsible JSON tree.
 *
 * `Node` renders one value and calls itself for that value's children, so the
 * data's shape becomes the element tree's shape. See
 * docs/learn/16-recursive-rendering.md.
 */

// How deep to render before refusing to go further.
//
// This is a defence, not a preference. Rendering is a recursive function call,
// so the data's depth is the render stack's depth -- and the data comes from
// strangers. A hostile or merely pathological payload nested tens of thousands
// deep would overflow the stack, and a stack overflow takes the whole tab down
// rather than showing an error. The raw view is always available for anything
// past the cap.
const MAX_DEPTH = 64

// Depth beyond which containers start collapsed.
//
// Separate from MAX_DEPTH: that one is about not crashing, this one is about
// not rendering ten thousand components nobody asked to see. Collapsed
// children are NOT RENDERED, rather than hidden with CSS -- hiding them costs
// exactly as much as showing them, which is the whole problem.
const AUTO_COLLAPSE_DEPTH = 2

export function JsonTree({ value }: { value: unknown }) {
  return (
    <div className="tree">
      <Node value={value} depth={0} />
    </div>
  )
}

function Node({ value, name, depth }: { value: unknown; name?: string; depth: number }) {
  if (depth > MAX_DEPTH) {
    return (
      <div className="tnode">
        <Key name={name} />
        <span className="t-cap">… too deeply nested to render — see the raw view</span>
      </div>
    )
  }

  if (value === null) return <Leaf name={name} cls="t-null" text="null" />
  if (typeof value === 'boolean') return <Leaf name={name} cls="t-bool" text={String(value)} />
  if (typeof value === 'number') return <Leaf name={name} cls="t-num" text={String(value)} />
  if (typeof value === 'string') return <Leaf name={name} cls="t-str" text={JSON.stringify(value)} />

  if (Array.isArray(value)) {
    return (
      <Branch name={name} depth={depth} open="[" close="]" count={value.length} label="items">
        {/* Index as key is safe here ONLY because a captured request never
            changes after it arrives. In a reorderable list this would be a
            bug. */}
        {value.map((v, i) => (
          <Node key={i} value={v} name={String(i)} depth={depth + 1} />
        ))}
      </Branch>
    )
  }

  if (typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>)
    return (
      <Branch name={name} depth={depth} open="{" close="}" count={entries.length} label="keys">
        {entries.map(([k, v]) => (
          <Node key={k} value={v} name={k} depth={depth + 1} />
        ))}
      </Branch>
    )
  }

  // undefined, functions, symbols -- unreachable from JSON.parse, but the type
  // is `unknown` and silently rendering nothing would be worse than saying so.
  return <Leaf name={name} cls="t-null" text={String(value)} />
}

function Branch({
  name,
  depth,
  open,
  close,
  count,
  label,
  children,
}: {
  name?: string
  depth: number
  open: string
  close: string
  count: number
  label: string
  children: React.ReactNode
}) {
  const [collapsed, setCollapsed] = useState(depth >= AUTO_COLLAPSE_DEPTH && count > 0)

  if (count === 0) {
    return <Leaf name={name} cls="t-punct" text={open + close} />
  }

  return (
    <div className="tnode">
      <button
        type="button"
        className="t-toggle"
        aria-expanded={!collapsed}
        onClick={() => setCollapsed((c) => !c)}
      >
        <span className="t-arrow">{collapsed ? '▸' : '▾'}</span>
        <Key name={name} />
        <span className="t-punct">{open}</span>
        {collapsed && (
          <span className="t-count">
            {' '}
            {count} {label}{' '}
          </span>
        )}
        {collapsed && <span className="t-punct">{close}</span>}
      </button>

      {/* Children are absent from the tree when collapsed, not merely hidden.
          A 10k-key object costs 10k components either way if they are
          rendered. */}
      {!collapsed && (
        <>
          <div className="t-children">{children}</div>
          <span className="t-punct t-closer">{close}</span>
        </>
      )}
    </div>
  )
}

function Leaf({ name, cls, text }: { name?: string; cls: string; text: string }) {
  return (
    <div className="tnode t-leaf">
      <Key name={name} />
      <span className={cls}>{text}</span>
    </div>
  )
}

function Key({ name }: { name?: string }) {
  if (name === undefined) return null
  return (
    <>
      <span className="t-key">{name}</span>
      <span className="t-punct">: </span>
    </>
  )
}
