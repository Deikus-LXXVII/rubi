# Run: python3 -I gen.py ed25519_test.json cases.json rfc8032.txt vectors.json
# Inputs (download them; they are not kept here):
#   ed25519_test.json  https://github.com/C2SP/wycheproof/blob/main/testvectors_v1/ed25519_test.json
#   cases.json         https://github.com/novifinancial/ed25519-speccheck/blob/main/cases.json
#   rfc8032.txt        https://www.rfc-editor.org/rfc/rfc8032.txt
# Builds the Ed25519 vector file: RFC 8032 7.1, Wycheproof ed25519_test.json, ed25519-speccheck cases, and a few derived
# cases. "valid" is decided by a reference verifier written from RFC 8032 section 6 (strict decoding, cofactorless check).
import hashlib, json, re, sys
p = 2**255 - 19
L = 2**252 + 27742317777372353535851937790883648493
d = -121665 * pow(121666, p - 2, p) % p
I = pow(2, (p - 1) // 4, p)
def add(P, Q):
    A = (P[1]-P[0])*(Q[1]-Q[0]) % p; B = (P[1]+P[0])*(Q[1]+Q[0]) % p
    C = 2*P[3]*Q[3]*d % p; D = 2*P[2]*Q[2] % p
    E, F, G, H = B-A, D-C, D+C, B+A
    return (E*F % p, G*H % p, F*G % p, E*H % p)
def mul(s, P):
    Q = (0, 1, 1, 0)
    while s > 0:
        if s & 1: Q = add(Q, P)
        P = add(P, P); s >>= 1
    return Q
def eq(P, Q):
    return (P[0]*Q[2] - Q[0]*P[2]) % p == 0 and (P[1]*Q[2] - Q[1]*P[2]) % p == 0
def recover_x(y, sign):
    if y >= p: return None
    x2 = (y*y - 1) * pow(d*y*y + 1, p - 2, p) % p
    if x2 == 0:
        return None if sign else 0
    x = pow(x2, (p + 3) // 8, p)
    if (x*x - x2) % p != 0: x = x * I % p
    if (x*x - x2) % p != 0: return None
    if (x & 1) != sign: x = p - x
    return x
def decompress(s):
    if len(s) != 32: return None
    y = int.from_bytes(s, 'little'); sign = y >> 255; y &= (1 << 255) - 1
    x = recover_x(y, sign)
    if x is None: return None
    return (x, y, 1, x*y % p)
def verify(pk, msg, sig):
    if len(pk) != 32 or len(sig) != 64: return False
    A = decompress(pk)
    if A is None: return False
    R = decompress(sig[:32])
    if R is None: return False
    s = int.from_bytes(sig[32:], 'little')
    if s >= L: return False
    h = int.from_bytes(hashlib.sha512(sig[:32] + pk + msg).digest(), 'little') % L
    G = decompress(bytes([0x58] + [0x66]*31))
    return eq(mul(s, G), add(R, mul(h, A)))

wp_path, sc_path, rfc_path, out_path = sys.argv[1:5]
vec = []
def put(source, name, pk, msg, sig, expect=None):
    ok = verify(bytes.fromhex(pk), bytes.fromhex(msg), bytes.fromhex(sig))
    if expect is not None and ok != expect:
        sys.exit('%s %s: reference says %s, source says %s' % (source, name, ok, expect))
    vec.append({'source': source, 'name': name, 'pk': pk, 'msg': msg, 'sig': sig, 'valid': ok})

# RFC 8032 7.1
text = open(rfc_path).read()
sec = text[text.index('7.1.  Test Vectors for Ed25519\n\n   These'):text.index('7.2.  Test Vectors for Ed25519ctx\n')]
sec = '\n'.join(l for l in sec.split('\n') if not l.startswith('RFC 8032') and not l.startswith('Josefsson') and '\f' not in l)
rfc = []
for block in sec.split('-----TEST ')[1:]:
    name = block.split('\n', 1)[0].strip()
    def field(label, nxt):
        m = re.search(label + r'[^\n]*:\n(.*?)\n\s*' + nxt, block, re.S)
        return re.sub(r'\s+', '', m.group(1)) if m else ''
    pk = field('PUBLIC KEY', 'MESSAGE')
    msg = field('MESSAGE', 'SIGNATURE')
    sig = re.sub(r'\s+', '', block.split('SIGNATURE:', 1)[1])
    sig = sig[:128]
    rfc.append((name, pk, msg, sig))
    put('rfc8032', 'TEST ' + name, pk, msg, sig, True)
assert [r[0] for r in rfc] == ['1', '2', '3', '1024', 'SHA(abc)'], [r[0] for r in rfc]
# derived from RFC TEST 2: a flipped bit in R, in S, in the message, in the key; S + L (the same S modulo L, not canonical)
name, pk, msg, sig = rfc[1]
def flip(h, i):
    b = bytearray.fromhex(h); b[i] ^= 1; return b.hex()
put('derived', 'TEST 2, a bit of R flipped', pk, msg, flip(sig, 0), False)
put('derived', 'TEST 2, a bit of S flipped', pk, msg, flip(sig, 40), False)
put('derived', 'TEST 2, the message changed', pk, flip(msg, 0), sig, False)
put('derived', 'TEST 2, a bit of the key flipped', flip(pk, 0), msg, sig, False)
s = int.from_bytes(bytes.fromhex(sig[64:]), 'little')
put('derived', 'TEST 2, S + L', pk, msg, sig[:64] + (s + L).to_bytes(32, 'little').hex(), False)
put('derived', 'TEST 2, the signature one byte short', pk, msg, sig[:126], False)
put('derived', 'TEST 2, the signature one byte long', pk, msg, sig + '00', False)
put('derived', 'TEST 2, the key one byte short', pk[:62], msg, sig, False)
# a public key that is not canonical: y = p + 1 encodes the point with y = 1 (the neutral element) only non-canonically
nc = (p + 1).to_bytes(32, 'little').hex()
put('derived', 'a key with y >= p', nc, msg, sig, False)
# x = 0 with the sign bit set ("negative zero"): y = 1 with bit 255
nz = bytearray((1).to_bytes(32, 'little')); nz[31] |= 0x80
put('derived', 'a key with x = 0 and the sign bit set', nz.hex(), msg, sig, False)
# The neutral point (y = 1) as a key: then [k]A is neutral for every k, so R = [s]B with S = s verifies for any message.
# Canonical, it is a small-order key that a cofactorless check accepts; encoded with y = p + 1 it must be refused.
def encode(P):
    zi = pow(P[2], p - 2, p); x = P[0]*zi % p; y = P[1]*zi % p
    return (y | ((x & 1) << 255)).to_bytes(32, 'little').hex()
G = decompress(bytes([0x58] + [0x66]*31))
sc = 1234567
good = encode(mul(sc, G)) + sc.to_bytes(32, 'little').hex()
put('derived', 'the neutral point as the key, canonical', (1).to_bytes(32, 'little').hex(), '616263', good, True)
put('derived', 'the neutral point as the key, encoded with y = p + 1', (p + 1).to_bytes(32, 'little').hex(), '616263', good, False)
# R = the neutral point with S = 0 and the neutral key: canonical R verifies, R encoded with y = p + 1 must not
put('derived', 'R neutral, canonical', (1).to_bytes(32, 'little').hex(), '', (1).to_bytes(32, 'little').hex() + '00'*32, True)
# S = L with R neutral and the neutral key: [L]B is neutral, so only the rule S < L refuses it
put('derived', 'R neutral, S = L', (1).to_bytes(32, 'little').hex(), '', (1).to_bytes(32, 'little').hex() + L.to_bytes(32, 'little').hex(), False)
put('derived', 'R neutral, encoded with y = p + 1', (1).to_bytes(32, 'little').hex(), '', (p + 1).to_bytes(32, 'little').hex() + '00'*32, False)
# Wycheproof
w = json.load(open(wp_path))
for g in w['testGroups']:
    for t in g['tests']:
        want = {'valid': True, 'invalid': False}.get(t['result'])
        put('wycheproof', 'tcId %d (%s)' % (t['tcId'], ', '.join(t['flags']) or t['comment']), g['publicKey']['pk'], t['msg'], t['sig'], want)
# ed25519-speccheck
for i, c in enumerate(json.load(open(sc_path))):
    put('speccheck', 'case %d' % i, c['pub_key'], c['message'], c['signature'])
json.dump({'note': 'see README.md', 'vectors': vec}, open(out_path, 'w'), indent=1)
print(len(vec), sum(v['valid'] for v in vec))
for v in vec:
    if v['source'] == 'speccheck': print(v['name'], v['valid'])
