" ~/.vimrc on the devbox, from /etc/skel. Ported from the laptop's ~/.vimrc:
" GUI and macOS-only parts dropped, duplicate and conflicting settings merged.
" `make diff-dotfiles` shows drift against the original.

" Every autocmd below is in this group, so re-sourcing doesn't stack them.
augroup vimrc
  autocmd!
augroup END

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" General
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
set history=700
set number
filetype plugin indent on
syntax enable

" Reload files changed outside vim, checking when the terminal regains focus.
set autoread
autocmd vimrc FocusGained,BufEnter * silent! checktime

let mapleader = ","

nnoremap <leader>w :w!<cr>

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" User interface
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
set scrolloff=7
set wildmenu
set wildmode=longest:full,full
set wildignore=*.o,*~,*.pyc
set cmdheight=2
set showcmd
set showmode
set ruler
set hidden
set display=lastline

set backspace=eol,start,indent
set whichwrap+=<,>,h,l

set ignorecase
set smartcase
set hlsearch
set incsearch
set magic

set showmatch
set matchtime=2

set lazyredraw
" Syntax highlighting on huge lines too; no column limit.
set synmaxcol=0

" No bells, audible or visual.
set belloff=all

" Leader sequences wait 500ms; escape sequences (Esc, Alt keys) only 10ms,
" matching tmux's escape-time, so Esc leaves insert mode at once.
set timeoutlen=500
set ttimeout
set ttimeoutlen=10

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Colors and encoding
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
set background=dark
"colorscheme elflord

set encoding=utf8
set fileformats=unix,dos

" Trailing whitespace, and spaces before a tab, in red. :match is per window,
" so it is set again for every window a buffer is shown in.
highlight ExtraWhitespace ctermbg=red guibg=red
autocmd vimrc ColorScheme * highlight ExtraWhitespace ctermbg=red guibg=red
autocmd vimrc BufWinEnter * match ExtraWhitespace /\s\+$\| \+\ze\t/
" Not while typing at the end of a line.
autocmd vimrc InsertEnter * match ExtraWhitespace /\s\+\%#\@<!$\| \+\ze\t/
autocmd vimrc InsertLeave * match ExtraWhitespace /\s\+$\| \+\ze\t/

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Files, backups and undo
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
set nobackup
set nowritebackup
set noswapfile

" With no swap or backup files, keep undo history across sessions instead.
if has('persistent_undo')
  let s:undodir = expand('~/.vim/undo')
  if !isdirectory(s:undodir)
    call mkdir(s:undodir, 'p', 0700)
  endif
  let &undodir = s:undodir
  set undofile
endif

" Remember open buffers on close.
set viminfo^=%

" Return to the last edit position when opening a file, except commit messages.
autocmd vimrc BufReadPost *
      \ if &filetype !~# 'commit' && line("'\"") > 0 && line("'\"") <= line("$") |
      \   execute "normal! g`\"" |
      \ endif

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Text, tabs and indent
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
set expandtab
set tabstop=2
set shiftwidth=2
set autoindent

set wrap
set linebreak

autocmd vimrc FileType make setlocal noexpandtab
autocmd vimrc FileType c setlocal tabstop=4 shiftwidth=4
autocmd vimrc BufNewFile,BufRead *.groff setfiletype groff

" Spell checking in prose only, not in code. ,ss toggles it anywhere.
autocmd vimrc FileType markdown,text,gitcommit,tex,groff setlocal spell

" Paste mode, for terminals that don't bracket pastes.
set pastetoggle=<F2>

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Moving around, tabs, windows and buffers
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" j and k move by screen line through wrapped text, but by real line with a
" count, so 5j still lands where relativenumber-style counting says.
nnoremap <expr> j v:count ? 'j' : 'gj'
nnoremap <expr> k v:count ? 'k' : 'gk'
xnoremap <expr> j v:count ? 'j' : 'gj'
xnoremap <expr> k v:count ? 'k' : 'gk'

" <Space> searches forward, Ctrl-<Space> backward (terminals send it as C-@).
noremap <space> /
noremap <c-space> ?
noremap <c-@> ?

nnoremap <silent> <leader><cr> :nohlsearch<cr>

nnoremap <C-j> <C-W>j
nnoremap <C-k> <C-W>k
nnoremap <C-h> <C-W>h
nnoremap <C-l> <C-W>l

" Close the current buffer without closing its window, or all buffers.
nnoremap <leader>bd :Bclose<cr>
nnoremap <leader>ba :%bdelete!<cr>

nnoremap <leader>tn :tabnew<cr>
nnoremap <leader>to :tabonly<cr>
nnoremap <leader>tc :tabclose<cr>
nnoremap <leader>tm :tabmove<space>
" Open a new tab in the current buffer's directory.
nnoremap <leader>te :tabedit <c-r>=expand("%:p:h")<cr>/

" Switch CWD to the directory of the open buffer.
nnoremap <leader>cd :cd %:p:h<cr>:pwd<cr>

set switchbuf=useopen,usetab,newtab
set showtabline=2

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Status line
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
set laststatus=2
set statusline=\ %{HasPaste()}%F%m%r%h%w\ \ CWD:\ %{getcwd()}\ \ \ Line:\ %l/%L\ \ Col:\ %c

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Editing mappings
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" 0 goes to the first non-blank character.
noremap 0 ^

" Alt-j and Alt-k move the line or selection. Terminals send Alt as an Esc
" prefix, which vim only reads as Alt once told to.
execute "set <M-j>=\ej"
execute "set <M-k>=\ek"
nnoremap <M-j> :move .+1<cr>==
nnoremap <M-k> :move .-2<cr>==
xnoremap <M-j> :move '>+1<cr>gv=gv
xnoremap <M-k> :move '<-2<cr>gv=gv

" Strip trailing whitespace on save, keeping the cursor and the last search.
function! DeleteTrailingWS()
  let l:view = winsaveview()
  keeppatterns %s/\s\+$//e
  call winrestview(l:view)
endfunction
autocmd vimrc BufWritePre *.py,*.coffee call DeleteTrailingWS()

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Visual selection search, vimgrep and quickfix
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" * and # search for the selection, gv vimgreps for it, ,r substitutes it.
xnoremap <silent> * :<C-u>call VisualSelection('f')<CR>
xnoremap <silent> # :<C-u>call VisualSelection('b')<CR>
xnoremap <silent> gv :<C-u>call VisualSelection('gv')<CR>
xnoremap <silent> <leader>r :<C-u>call VisualSelection('replace')<CR>

" vimgrep across the tree, or in the current file.
nnoremap <leader>g :vimgrep // **/*.<left><left><left><left><left><left><left>
nnoremap <leader><space> :vimgrep // %<left><left><left>

" Quickfix: ,cc opens it, ,co copies it to a new tab, ,n and ,p step through.
nnoremap <leader>cc :botright copen<cr>
nnoremap <leader>co ggVGy:tabnew<cr>:set syntax=qf<cr>pgg
nnoremap <leader>n :cnext<cr>
nnoremap <leader>p :cprevious<cr>

set grepprg=grep\ -nH\ $*

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Spell checking
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
nnoremap <leader>ss :setlocal spell!<cr>
nnoremap <leader>sn ]s
nnoremap <leader>sp [s
nnoremap <leader>sa zg
nnoremap <leader>s? z=

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Misc
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Remove Windows ^M line endings.
nnoremap <leader>m mmHmt:%s/\r$//e<cr>'tzt'm

" Scratch buffer.
nnoremap <leader>q :e ~/buffer<cr>

nnoremap <leader>pp :setlocal paste!<cr>

let g:tex_flavor = 'latex'

""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
" Helper functions
""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""""
function! VisualSelection(direction)
  let l:saved_reg = @"
  execute "normal! gvy"
  let l:pattern = escape(@", '\\/.*$^~[]')
  let l:pattern = substitute(l:pattern, "\n$", "", "")
  let @" = l:saved_reg

  if a:direction ==# 'gv'
    call feedkeys(":vimgrep /" . l:pattern . "/ **/*.", 'n')
    return
  elseif a:direction ==# 'replace'
    call feedkeys(":%s/" . l:pattern . "/", 'n')
    return
  endif
  let @/ = l:pattern
  call histadd('search', l:pattern)
  set hlsearch
  execute "normal! " . (a:direction ==# 'b' ? 'N' : 'n')
endfunction

function! HasPaste()
  return &paste ? 'PASTE MODE  ' : ''
endfunction

" Delete a buffer without closing its window.
command! Bclose call <SID>BufcloseCloseIt()
function! <SID>BufcloseCloseIt()
  let l:currentBufNum = bufnr("%")
  let l:alternateBufNum = bufnr("#")

  if buflisted(l:alternateBufNum)
    buffer #
  else
    bnext
  endif

  if bufnr("%") == l:currentBufNum
    new
  endif

  if buflisted(l:currentBufNum)
    execute "bdelete! " . l:currentBufNum
  endif
endfunction

" :Csv N highlights column N of a CSV file; :Csv 0 clears it.
function! CSVH(colnr)
  if a:colnr > 1
    let n = a:colnr - 1
    execute 'match Keyword /^\([^,]*,\)\{' . n . '}\zs[^,]*/'
    execute 'normal! 0' . n . 'f,'
  elseif a:colnr == 1
    match Keyword /^[^,]*/
    normal! 0
  else
    match
  endif
endfunction
command! -nargs=1 Csv call CSVH(<args>)
