import { mount } from 'svelte';
import Board from './Board.svelte';
import { connectApp } from './bridge';

mount(Board, { target: document.getElementById('app')!, props: { connect: connectApp } });
