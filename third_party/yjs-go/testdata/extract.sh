#!/usr/bin/env bash
# extract.sh — Generate Yjs test vectors from the official yjs package.
# Run this script to (re)generate the golden files in testdata/yjs-vectors/.
# Requires Node.js + npm.

set -euo pipefail
cd "$(dirname "$0")"

echo "Installing yjs + y-protocols..."
mkdir -p _node
cd _node
cat > package.json <<'EOF'
{
  "name": "yjs-extract",
  "version": "1.0.0",
  "type": "module",
  "dependencies": {
    "yjs": "*",
    "y-protocols": "*"
  }
}
EOF
npm install --silent
cd ..

echo "Generating test vectors..."
node --input-type=module <<'NODESCRIPT'
import * as Y from './_node/node_modules/yjs/src/index.js';
import * as encoding from './_node/node_modules/lib0/encoding.js';
import * as decoding from './_node/node_modules/lib0/decoding.js';
import * as syncProtocol from './_node/node_modules/y-protocols/sync.js';
import * as awarenessProtocol from './_node/node_modules/y-protocols/awareness.js';
import { writeFileSync, mkdirSync } from 'fs';

mkdirSync('yjs-vectors', { recursive: true });

// Vector 1: empty doc state vector
{
  const doc = new Y.Doc();
  const sv = Y.encodeStateVector(doc);
  writeFileSync('yjs-vectors/empty-state-vector.bin', Buffer.from(sv));
  console.log('empty-state-vector.bin:', sv.length, 'bytes');
}

// Vector 2: empty doc update (state as update from nothing)
{
  const doc = new Y.Doc();
  const update = Y.encodeStateAsUpdate(doc);
  writeFileSync('yjs-vectors/empty-update.bin', Buffer.from(update));
  console.log('empty-update.bin:', update.length, 'bytes');
}

// Vector 3: single text insert
{
  const doc = new Y.Doc({ clientID: 1 });
  const text = doc.getText('content');
  doc.transact(() => { text.insert(0, 'hello'); });
  const update = Y.encodeStateAsUpdate(doc);
  const sv = Y.encodeStateVector(doc);
  writeFileSync('yjs-vectors/text-insert-hello.bin', Buffer.from(update));
  writeFileSync('yjs-vectors/text-insert-hello-sv.bin', Buffer.from(sv));
  console.log('text-insert-hello.bin:', update.length, 'bytes, text:', text.toString());
}

// Vector 4: two concurrent inserts converging
{
  const docA = new Y.Doc({ clientID: 1 });
  const docB = new Y.Doc({ clientID: 2 });
  const textA = docA.getText('content');
  const textB = docB.getText('content');
  docA.transact(() => { textA.insert(0, 'aaa'); });
  docB.transact(() => { textB.insert(0, 'bbb'); });
  const updateA = Y.encodeStateAsUpdate(docA);
  const updateB = Y.encodeStateAsUpdate(docB);
  Y.applyUpdate(docA, updateB);
  Y.applyUpdate(docB, updateA);
  if (textA.toString() !== textB.toString()) {
    throw new Error('convergence failed: ' + textA.toString() + ' vs ' + textB.toString());
  }
  writeFileSync('yjs-vectors/concurrent-insert-a.bin', Buffer.from(updateA));
  writeFileSync('yjs-vectors/concurrent-insert-b.bin', Buffer.from(updateB));
  writeFileSync('yjs-vectors/concurrent-insert-merged.txt', textA.toString());
  console.log('concurrent-insert merged:', textA.toString());
}

// Vector 5: map set
{
  const doc = new Y.Doc({ clientID: 1 });
  const map = doc.getMap('data');
  doc.transact(() => { map.set('key', 'value'); });
  const update = Y.encodeStateAsUpdate(doc);
  writeFileSync('yjs-vectors/map-set.bin', Buffer.from(update));
  console.log('map-set.bin:', update.length, 'bytes, map:', JSON.stringify(map.toJSON()));
}

// Vector 6: sync step 1 message
{
  const doc = new Y.Doc({ clientID: 1 });
  const text = doc.getText('content');
  doc.transact(() => { text.insert(0, 'sync test'); });
  const enc = encoding.createEncoder();
  syncProtocol.writeSyncStep1(enc, doc);
  const msg = encoding.toUint8Array(enc);
  writeFileSync('yjs-vectors/sync-step1.bin', Buffer.from(msg));
  console.log('sync-step1.bin:', msg.length, 'bytes');
}

// Vector 7: sync step 2 message
{
  const doc = new Y.Doc({ clientID: 1 });
  const text = doc.getText('content');
  doc.transact(() => { text.insert(0, 'sync test'); });
  const enc = encoding.createEncoder();
  syncProtocol.writeSyncStep2(enc, doc);
  const msg = encoding.toUint8Array(enc);
  writeFileSync('yjs-vectors/sync-step2.bin', Buffer.from(msg));
  console.log('sync-step2.bin:', msg.length, 'bytes');
}

// Vector 8: awareness update (encode manually to avoid constructor check issue)
{
  const clientID = 42;
  const state = { name: 'Alice', cursor: { line: 1, ch: 5 } };
  const clock = 1;
  // Encode manually: varuint(1), varuint(clientID), varuint(clock), varstring(JSON(state))
  const enc = encoding.createEncoder();
  encoding.writeVarUint(enc, 1);       // 1 client
  encoding.writeVarUint(enc, clientID);
  encoding.writeVarUint(enc, clock);
  encoding.writeVarString(enc, JSON.stringify(state));
  const update = encoding.toUint8Array(enc);
  writeFileSync('yjs-vectors/awareness-update.bin', Buffer.from(update));
  console.log('awareness-update.bin:', update.length, 'bytes');
  writeFileSync('yjs-vectors/awareness-update-state.json', JSON.stringify(state));
}

// Vector 9: text delete
{
  const doc = new Y.Doc({ clientID: 1 });
  const text = doc.getText('content');
  doc.transact(() => { text.insert(0, 'hello world'); });
  const updateBefore = Y.encodeStateAsUpdate(doc);
  doc.transact(() => { text.delete(5, 6); });
  const updateAfter = Y.encodeStateAsUpdate(doc, updateBefore);
  writeFileSync('yjs-vectors/text-delete-update.bin', Buffer.from(updateAfter));
  writeFileSync('yjs-vectors/text-delete-expected.txt', text.toString());
  console.log('text-delete result:', text.toString());
}

console.log('All vectors written to testdata/yjs-vectors/');
NODESCRIPT
